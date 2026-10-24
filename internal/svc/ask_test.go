package svc_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alash3al/hfy/internal/svc"
)

func askQuestions(t *testing.T) []svc.Question {
	t.Helper()
	qs, err := svc.ParseQuestions(json.RawMessage(`{"questions":[
		{"id":"size","text":"Which size?","type":"single","options":[{"value":"m","label":"Medium"},{"value":"l"}]},
		{"id":"extras","text":"Extras?","type":"multi","options":[{"value":"gift"},{"value":"wrap"}]},
		{"id":"note","text":"Note?","type":"text","required":false},
		{"id":"ok","text":"Proceed?","type":"confirm"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return qs
}

func TestParseQuestions(t *testing.T) {
	for _, bad := range []string{
		`{}`,
		`{"questions":[{}]}`,
		`{"questions":[{"id":"Size","text":"x","type":"single"}]}`,
		`{"questions":[{"id":"a","text":"x","type":"single"},{"id":"a","text":"y","type":"text"}]}`,
		`{"questions":[{"id":"a","text":"","type":"text"}]}`,
		`{"questions":[{"id":"a","text":"` + strings.Repeat("x", 301) + `","type":"text"}]}`,
		`{"questions":[{"id":"a","text":"x"}]}`,
		`{"questions":[{"id":"a","text":"x","type":"nonsense"}]}`,
		`{"questions":[{"id":"a","text":"x","type":"multi","options":[{"value":"m"}]}]}`,
		`{"questions":[{"id":"a","text":"x","type":"single","options":[{"value":"m"},{"value":"m"}]}]}`,
		`{"questions":[{"id":"a","text":"x","type":"text","options":[{"value":"m"},{"value":"l"}]}]}`,
		`{"questions":[` + strings.Repeat(`{"id":"a","text":"x","type":"text"},`, 10) + `{"id":"b","text":"x","type":"text"}]}`,
	} {
		if _, err := svc.ParseQuestions(json.RawMessage(bad)); err == nil {
			t.Errorf("%.64s: must be refused", bad)
		}
	}

	questions := askQuestions(t)
	if len(questions) != 4 || strings.Join(questions[0].Values(), ",") != "m,l" ||
		questions[0].Options[0].Label != "Medium" || questions[1].Values()[1] != "wrap" ||
		questions[2].Required || questions[3].Type != "confirm" {
		t.Fatalf("questions: %+v", questions)
	}
}

// Valid answers become the canonical object the approval.decided payload
// stores and the tool result serves as JSON text after {"answers":.
func TestParseAnswersValid(t *testing.T) {
	qs := askQuestions(t)
	for answers, want := range map[string]string{
		`{"size":"m","extras":["gift"],"ok":true}`:                    `{"extras":["gift"],"ok":true,"size":"m"}`,
		`{"size":"l","extras":["gift","wrap"],"ok":false}`:            `{"extras":["gift","wrap"],"ok":false,"size":"l"}`,
		`{"size":"m","extras":["gift"],"note":"a\u0000b","ok":false}`: `{"extras":["gift"],"note":"ab","ok":false,"size":"m"}`,
		`{"size":"l","extras":["wrap"],"note":"","ok":true}`:          `{"extras":["wrap"],"ok":true,"size":"l"}`, // skipped optional text: empty
	} {
		out, err := svc.ParseAnswers(qs, json.RawMessage(answers))
		if err != nil {
			t.Errorf("%s: %v", answers, err)
		} else if string(out) != want {
			t.Errorf("%s: got %s, want %s", answers, out, want)
		}
	}
}

// Every violation is a client's mistake: a 400 naming the question id.
func TestParseAnswersRejected(t *testing.T) {
	qs := askQuestions(t)
	for name, raw := range map[string]string{
		"missing required":      `{}`,
		"unknown id":            `{"unknown":"x"}`,
		"bad option":            `{"size":"xl"}`,
		"option must be string": `{"size":1}`,
		"multi must be array":   `{"size":"m","extras":"gift","ok":true}`,
		"multi subset":          `{"size":"m","extras":["gift","refund"],"ok":true}`,
		"multi element type":    `{"size":"m","extras":[7],"ok":true}`,
		"required multi empty":  `{"size":"m","extras":[],"ok":true}`,
		"text too long":         `{"size":"m","note":"` + strings.Repeat("a", 2001) + `","ok":true}`,
		"confirm must be bool":  `{"size":"m","ok":"yes"}`,
		"not an object":         `["m"]`,
		"null":                  `null`,
	} {
		_, err := svc.ParseAnswers(qs, json.RawMessage(raw))
		var bad *svc.BadRequestError
		if err == nil || !errors.As(err, &bad) || !strings.Contains(err.Error(), "answers") {
			t.Errorf("%s (%.48s): want a 400 naming the problem, got %v", name, raw, err)
		}
	}

	_, err := svc.ParseAnswers(qs, json.RawMessage(`{"note":"`+strings.Repeat("a", 40_000)+`"}`))
	var bad *svc.BadRequestError
	if err == nil || !errors.As(err, &bad) || !strings.Contains(err.Error(), "answers exceed") {
		t.Errorf("oversized answers: %v", err)
	}
}
