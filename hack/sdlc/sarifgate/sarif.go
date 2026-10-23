package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// The subset of SARIF 2.1.0 that CodeQL writes and the gate needs.
type sarifLog struct {
	Runs []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver     sarifComponent   `json:"driver"`
	Extensions []sarifComponent `json:"extensions"`
}

type sarifComponent struct {
	Name  string      `json:"name"`
	Rules []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID                   string `json:"id"`
	DefaultConfiguration struct {
		Level string `json:"level"`
	} `json:"defaultConfiguration"`
	Properties map[string]any `json:"properties"`
}

type sarifResult struct {
	RuleID  string `json:"ruleId"`
	Level   string `json:"level"`
	Message struct {
		Text string `json:"text"`
	} `json:"message"`
	Locations []struct {
		PhysicalLocation struct {
			ArtifactLocation struct {
				URI string `json:"uri"`
			} `json:"artifactLocation"`
			Region struct {
				StartLine int `json:"startLine"`
			} `json:"region"`
		} `json:"physicalLocation"`
	} `json:"locations"`
	PartialFingerprints map[string]string `json:"partialFingerprints"`
	Suppressions        []struct {
		Kind string `json:"kind"`
	} `json:"suppressions"`
}

// result is one alert, resolved against its rule.
type result struct {
	Tool        string
	Rule        string
	Level       string // error, warning, note or none
	Severity    float64
	Path        string
	Line        int
	Message     string
	Fingerprint string
	Suppressed  bool
}

func readSARIF(path string) ([]result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var log sarifLog
	if err := json.Unmarshal(data, &log); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var out []result
	for _, run := range log.Runs {
		rules := map[string]sarifRule{}
		for _, c := range append([]sarifComponent{run.Tool.Driver}, run.Tool.Extensions...) {
			for _, r := range c.Rules {
				rules[r.ID] = r
			}
		}
		for _, r := range run.Results {
			rule := rules[r.RuleID]
			res := result{
				Tool:        run.Tool.Driver.Name,
				Rule:        r.RuleID,
				Level:       r.Level,
				Severity:    securitySeverity(rule),
				Message:     r.Message.Text,
				Fingerprint: r.PartialFingerprints["primaryLocationLineHash"],
				Suppressed:  len(r.Suppressions) > 0,
			}
			if res.Level == "" {
				res.Level = rule.DefaultConfiguration.Level
			}
			if res.Level == "" {
				res.Level = "warning" // the SARIF default
			}
			if len(r.Locations) > 0 {
				res.Path = r.Locations[0].PhysicalLocation.ArtifactLocation.URI
				res.Line = r.Locations[0].PhysicalLocation.Region.StartLine
			}
			out = append(out, res)
		}
	}
	return out, nil
}

// securitySeverity returns the rule's CVSS-like "security-severity" property, 0 when absent.
func securitySeverity(r sarifRule) float64 {
	switch v := r.Properties["security-severity"].(type) {
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	case float64:
		return v
	}
	return 0
}
