// Package bot contains the core logic for TorontoBot which is shared across host platforms.
package bot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"text/template"
	"time"

	"github.com/sashabaranov/go-openai"

	"github.com/geomodulus/citygraph"
	"github.com/geomodulus/torontobot/db/reader"
	"github.com/geomodulus/torontobot/viz"
)

const (
	// Model is the openai model to query. GPT-4 is expensive, so we use GPT-3.5.
	Model = openai.GPT3Dot5Turbo
	// RespTemp is the response temperature we want from the model. Default temp is 1.0 and higher
	// is more "creative".
	RespTemp = 0.1
)

type TorontoBot struct {
	Hostname          string
	sqlGenPrompt      *template.Template
	chartSelectPrompt *template.Template
	graphStore        *citygraph.Store
	ai                *openai.Client
	db                *sql.DB
}

func New(db *sql.DB, ai *openai.Client, store *citygraph.Store, host string) (*TorontoBot, error) {
	sqlGenPrompt, err := template.ParseFiles("./prompts/sql_gen.txt")
	if err != nil {
		return nil, fmt.Errorf("parsing sql_gen.txt: %v", err)
	}
	chartSelectPrompt, err := template.ParseFiles("./prompts/chart_select.txt")
	if err != nil {
		return nil, fmt.Errorf("parsing chart_select.txt: %v", err)
	}

	return &TorontoBot{
		Hostname:          host,
		sqlGenPrompt:      sqlGenPrompt,
		chartSelectPrompt: chartSelectPrompt,
		graphStore:        store,
		ai:                ai,
		db:                db,
	}, nil
}

type SQLResponse struct {
	Schema        string
	Applicability string
	SQL           string
	MissingData   string
}

func (b *TorontoBot) SQLAnalysis(ctx context.Context, question string) (*SQLResponse, error) {
	var query bytes.Buffer
	data := struct {
		Date    string
		Command string
	}{
		Date:    time.Now().Format("January 2, 2006"),
		Command: question,
	}
	if err := b.sqlGenPrompt.Execute(&query, data); err != nil {
		return nil, fmt.Errorf("executing template: %+v", err)
	}
	log.Printf("sending request to openai: %q\n", query.String())
	aiResp, err := b.ai.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: Model,
		Messages: []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: query.String(),
		}},
		Temperature: RespTemp,
	})
	if err != nil {
		return nil, fmt.Errorf("CreateChatCompletion: %v", err)
	}
	log.Printf("Got reply: %s\n", aiResp.Choices[0].Message.Content)

	var resp SQLResponse
	if err := json.Unmarshal([]byte(aiResp.Choices[0].Message.Content), &resp); err != nil {
		return nil, fmt.Errorf("unmarshalling response %q: %v", aiResp.Choices[0].Message.Content, err)
	}
	return &resp, nil
}

func (b *TorontoBot) LoadResults(sqlQuery string) (string, error) {
	fmt.Println("running sqlQuery:", sqlQuery)
	return reader.ReadDataTable(b.db, sqlQuery)
}

type ChartSelectResponse struct {
	Chart           string
	Title           string
	Data            []*viz.DataEntry
	ValueIsCurrency bool
}

type ChartType int

const (
	ChartTypeUnknown ChartType = iota
	ChartTypeBar
	ChartTypeLine
	ChartTypePie
	ChartTypeScatter
)

func (b *TorontoBot) SelectChart(ctx context.Context, question, dataTable string) (*ChartSelectResponse, error) {
	var query bytes.Buffer
	data := struct {
		Title string
		Data  string
	}{
		Title: question,
		Data:  dataTable,
	}
	if err := b.chartSelectPrompt.Execute(&query, data); err != nil {
		return nil, fmt.Errorf("executing template: %+v", err)
	}
	log.Printf("sending request to openai: %q\n", query.String())
	aiResp, err := b.ai.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: Model,
		Messages: []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: query.String(),
		}},
		Temperature: RespTemp,
	})
	if err != nil {
		return nil, fmt.Errorf("CreateChatCompletion: %v", err)
	}
	log.Printf("Got reply: %s\n", aiResp.Choices[0].Message.Content)

	var resp ChartSelectResponse
	if err := json.Unmarshal([]byte(aiResp.Choices[0].Message.Content), &resp); err != nil {
		return nil, fmt.Errorf("unmarshalling response %q: %v", aiResp.Choices[0].Message.Content, err)
	}

	fmt.Printf("%+v\n", resp)

	return &resp, nil
}

func (b *TorontoBot) HasGraphStore() bool {
	return b.graphStore != nil
}

func (b *TorontoBot) SaveToGraph(ctx context.Context, id, title, body, js, featureImage, user string) (string, error) {
	camera := map[string]interface{}{
		"": map[string]interface{}{
			"center":  map[string]float64{"lng": -79.384, "lat": 43.645},
			"zoom":    13.8,
			"pitch":   0,
			"bearing": -30,
		}}
	mod := &citygraph.Module{
		ID:           id,
		Name:         title,
		Headline:     fmt.Sprintf("<h1>City Budget: %s</h1>", title),
		Categories:   []string{"Open Data"},
		Creators:     []string{user},
		Camera:       camera,
		FeatureImage: featureImage,
		Description:  "User-generated open data visualization",
		PubDate:      time.Now().Format("2006-01-02"),
		CodeCredit:   "TorontoBot, an open data bot",
	}
	if err := b.graphStore.WriteModule(ctx, mod); err != nil {
		return "", fmt.Errorf("writing module: %v", err)
	}

	q, err := mod.VertexQuery()
	if err != nil {
		return "", fmt.Errorf("generating vertex query: %v", err)
	}
	if err := b.graphStore.WriteBodyText(ctx, q, body); err != nil {
		return "", fmt.Errorf("writing body text: %v", err)
	}

	js += "\n\nmodule.initAdUnits();"
	if err := b.graphStore.WriteJS(ctx, q, js); err != nil {
		return "", fmt.Errorf("writing JS: %v", err)
	}

	slugID, err := mod.SlugID()
	if err != nil {
		return "", fmt.Errorf("generating slug ID: %v", err)
	}
	return fmt.Sprintf("/mod/%s/%s", slugID, mod.SlugTitle()), nil
}
