package domain

import (
	"encoding/json"
	"testing"
	"time"
)

// Cycle 1: AgentDefinition legacy JSON (no new fields) deserialises with zero values.
func TestAgentDefinition_LegacyJSON_ZeroValuesForNewFields(t *testing.T) {
	raw := `{"ID":"agent-1","Name":"searcher","Description":"search agent","Runtime":"python","ExecPath":"/agents/search.py","Port":"50052","Dir":"/agents"}`
	var got AgentDefinition
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if got.SourceHash != "" {
		t.Errorf("expected empty SourceHash, got %q", got.SourceHash)
	}
	if got.ManifestVersion != "" {
		t.Errorf("expected empty ManifestVersion, got %q", got.ManifestVersion)
	}
	if got.Provisional != false {
		t.Errorf("expected Provisional=false, got %v", got.Provisional)
	}
}

// Cycle 2: AgentDefinition with all new fields round-trips through JSON correctly.
func TestAgentDefinition_NewFields_RoundTrip(t *testing.T) {
	original := AgentDefinition{
		ID:              "agent-2",
		Name:            "analyst",
		SourceHash:      "abc123",
		ManifestVersion: "1.2.0",
		Provisional:     true,
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var got AgentDefinition
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if got.SourceHash != "abc123" {
		t.Errorf("SourceHash: expected %q, got %q", "abc123", got.SourceHash)
	}
	if got.ManifestVersion != "1.2.0" {
		t.Errorf("ManifestVersion: expected %q, got %q", "1.2.0", got.ManifestVersion)
	}
	if !got.Provisional {
		t.Errorf("Provisional: expected true, got false")
	}
}

// Cycle 3: AuctionTask with RequiredFormats round-trips correctly.
func TestAuctionTask_RequiredFormats_RoundTrip(t *testing.T) {
	original := AuctionTask{
		ID:              "task-1",
		Description:     "summarise document",
		RequiredFormats: []string{"markdown", "json"},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var got AuctionTask
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(got.RequiredFormats) != 2 || got.RequiredFormats[0] != "markdown" || got.RequiredFormats[1] != "json" {
		t.Errorf("RequiredFormats round-trip failed: got %v", got.RequiredFormats)
	}
}

// Cycle 4: AgentManifest round-trips correctly.
func TestAgentManifest_RoundTrip(t *testing.T) {
	original := AgentManifest{
		Version:          "2.0.0",
		Tools:            []string{"search", "embed"},
		SupportedFormats: []string{"json", "text"},
		InputSchema:      map[string]any{"query": "string"},
		OutputSchema:     map[string]any{"result": "string"},
		ReleaseNotes:     "initial release",
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var got AgentManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if got.Version != "2.0.0" {
		t.Errorf("Version: expected %q, got %q", "2.0.0", got.Version)
	}
	if len(got.Tools) != 2 || got.Tools[0] != "search" {
		t.Errorf("Tools round-trip failed: got %v", got.Tools)
	}
	if len(got.SupportedFormats) != 2 || got.SupportedFormats[1] != "text" {
		t.Errorf("SupportedFormats round-trip failed: got %v", got.SupportedFormats)
	}
	if got.InputSchema["query"] != "string" {
		t.Errorf("InputSchema round-trip failed: got %v", got.InputSchema)
	}
	if got.OutputSchema["result"] != "string" {
		t.Errorf("OutputSchema round-trip failed: got %v", got.OutputSchema)
	}
	if got.ReleaseNotes != "initial release" {
		t.Errorf("ReleaseNotes: expected %q, got %q", "initial release", got.ReleaseNotes)
	}
}

// Cycle 5: AgentProfile round-trips correctly.
func TestAgentProfile_RoundTrip(t *testing.T) {
	original := AgentProfile{
		AgentID:                    "agent-3",
		SourceHash:                 "deadbeef",
		SuccessRate:                0.92,
		TrustScore:                 0.87,
		NetworkLatencyMedianMs:     45,
		ComputationLatencyMedianMs: 120,
		ContextGrowthBytesMedian:   512,
		Provisional:                false,
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var got AgentProfile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if got.AgentID != "agent-3" {
		t.Errorf("AgentID: expected %q, got %q", "agent-3", got.AgentID)
	}
	if got.SourceHash != "deadbeef" {
		t.Errorf("SourceHash: expected %q, got %q", "deadbeef", got.SourceHash)
	}
	if got.SuccessRate != 0.92 {
		t.Errorf("SuccessRate: expected 0.92, got %v", got.SuccessRate)
	}
	if got.TrustScore != 0.87 {
		t.Errorf("TrustScore: expected 0.87, got %v", got.TrustScore)
	}
	if got.NetworkLatencyMedianMs != 45 {
		t.Errorf("NetworkLatencyMedianMs: expected 45, got %v", got.NetworkLatencyMedianMs)
	}
	if got.ComputationLatencyMedianMs != 120 {
		t.Errorf("ComputationLatencyMedianMs: expected 120, got %v", got.ComputationLatencyMedianMs)
	}
	if got.ContextGrowthBytesMedian != 512 {
		t.Errorf("ContextGrowthBytesMedian: expected 512, got %v", got.ContextGrowthBytesMedian)
	}
	if got.Provisional != false {
		t.Errorf("Provisional: expected false, got true")
	}
}

// Cycle 6: TaskEvent round-trips correctly including Timestamp.
func TestTaskEvent_RoundTrip(t *testing.T) {
	ts := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	original := TaskEvent{
		TaskID:               "task-42",
		AgentID:              "agent-7",
		SourceHash:           "cafebabe",
		BidConfidence:        0.95,
		VerifierScore:        0.88,
		NetworkLatencyMs:     30,
		ComputationLatencyMs: 200,
		ContextGrowthBytes:   1024,
		Timestamp:            ts,
		Verified:             true,
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var got TaskEvent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if got.TaskID != "task-42" {
		t.Errorf("TaskID: expected %q, got %q", "task-42", got.TaskID)
	}
	if got.AgentID != "agent-7" {
		t.Errorf("AgentID: expected %q, got %q", "agent-7", got.AgentID)
	}
	if got.SourceHash != "cafebabe" {
		t.Errorf("SourceHash: expected %q, got %q", "cafebabe", got.SourceHash)
	}
	if got.BidConfidence != 0.95 {
		t.Errorf("BidConfidence: expected 0.95, got %v", got.BidConfidence)
	}
	if got.VerifierScore != 0.88 {
		t.Errorf("VerifierScore: expected 0.88, got %v", got.VerifierScore)
	}
	if got.NetworkLatencyMs != 30 {
		t.Errorf("NetworkLatencyMs: expected 30, got %v", got.NetworkLatencyMs)
	}
	if got.ComputationLatencyMs != 200 {
		t.Errorf("ComputationLatencyMs: expected 200, got %v", got.ComputationLatencyMs)
	}
	if got.ContextGrowthBytes != 1024 {
		t.Errorf("ContextGrowthBytes: expected 1024, got %v", got.ContextGrowthBytes)
	}
	if !got.Timestamp.Equal(ts) {
		t.Errorf("Timestamp: expected %v, got %v", ts, got.Timestamp)
	}
	if !got.Verified {
		t.Errorf("Verified: expected true, got false")
	}
}
