package httpserver

import (
	"testing"
)

func TestParseMediaPathToken(t *testing.T) {
	linkToken, err := parseMediaPath("9f9f1d428f17/report.pdf")
	if err != nil {
		t.Fatalf("parseMediaPath returned error: %v", err)
	}
	if linkToken != "9f9f1d428f17" {
		t.Fatalf("unexpected token: %s", linkToken)
	}
}

func TestParseMediaPathRejectsLegacyFormat(t *testing.T) {
	if _, err := parseMediaPath("abc123456/report.pdf"); err == nil {
		t.Fatal("expected legacy path to be rejected")
	}
}
