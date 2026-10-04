package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

const workspace = "10000000-0000-4000-8000-000000000002"
const user = "10000000-0000-4000-8000-000000000001"
const agentID = "30000000-0000-4000-8000-000000000001"
const issueID = "40000000-0000-4000-8000-000000000001"
const taskID = "20000000-0000-4000-8000-000000000001"
const daemonID = "10000000-0000-4000-8000-000000000003"
const issuer = "http://keycloak:8080/realms/e2e"

type configuration struct{ JWT, Client, Master, Proxy, Reference string }

func load() (configuration, error) {
	var c configuration
	data, err := os.ReadFile("/config/control.json")
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	return c, err
}
func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: e2e init|setup|run|agent|mcp|check")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var err error
	switch os.Args[1] {
	case "health":
		var response *http.Response
		response, err = client.Get("http://127.0.0.1:8080/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode != 204 {
				err = fmt.Errorf("unhealthy")
			}
		}
	case "models":
		err = listModels(ctx)
	case "clean":
		err = reap(ctx)
	case "init":
		err = initialize()
	case "setup":
		err = setup(ctx)
	case "run":
		err = execute(ctx)
	case "agent":
		err = agent(ctx)
	case "mcp":
		err = serveMCP()
	case "check":
		err = check(ctx)
	default:
		err = fmt.Errorf("unknown mode")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
