package main

import "testing"

func TestInterpretURL(t *testing.T) {
	for in, want := range map[string]string{
		"example.com":              "https://example.com",
		"example.com/a?b=c":        "https://example.com/a?b=c",
		"https://example.com":      "https://example.com",
		"http://example.com":       "http://example.com",
		"localhost:8080/x":         "http://localhost:8080/x",
		"127.0.0.1:3000":           "http://127.0.0.1:3000",
		"myhost:8000":              "http://myhost:8000",
		"printer.local":            "http://printer.local",
		"about:blank":              "about:blank",
		"file:///etc/passwd":       "",
		"chrome://settings":        "",
		"chrome://inspect":         "",
		"javascript:alert(1)":      "",
		"view-source:https://a.b/": "",
		"data:text/html,hi":        "",
		"about:settings":           "",
		"not a url":                "",
		"nodots":                   "",
	} {
		got, err := interpretURL(in)
		if want == "" {
			if err == nil {
				t.Errorf("interpretURL(%q) = %q, want an error", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("interpretURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
