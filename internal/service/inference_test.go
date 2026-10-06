package service

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gently-whitesnow/multica-sandbox/internal/inference"
)

func TestDeployInferenceExamplesDecode(t *testing.T) {
	data, err := os.ReadFile("../../deploy/opencode.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var c OpenCodeConfig
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || c.InferenceFile == "" || c.InferenceRelay == nil || c.MulticaRelay == nil {
		t.Fatal("OpenCode example does not configure both relays")
	}
	path, _ := filepath.Abs("../../deploy/inference.example.json")
	config, err := inference.ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = inference.New(config, "https://multica.example.com"); err != nil {
		t.Fatal("inference example rejected", err)
	}
}

func TestInferenceRequiresRelay(t *testing.T) {
	for _, c := range []OpenCodeConfig{{InferenceFile: "/etc/multica-sandbox/inference.json"}, {InferenceRelay: &RelayConfig{Listen: ":8092", URL: "http://inference-relay:8092"}}} {
		if (c.InferenceFile == "") == (c.InferenceRelay == nil) {
			t.Fatal("fixture must configure exactly one side")
		}
		if _, _, err := openCodeInference(t.Context(), Config{Server: "https://multica.example.com", OpenCode: &c}); err == nil {
			t.Fatal("inference without its relay accepted")
		}
	}
}
