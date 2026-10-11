package inference

import "strings"

// Wire APIs a gateway model is reached through. Chat is the OpenAI-compatible default;
// Responses and Messages keep native prompt caching and reasoning state between turns.
const (
	APIChat      = "chat"
	APIResponses = "responses"
	APIMessages  = "messages"
)

// OpenCode provider IDs, and so the model ID prefixes in Multica's picker. The chat
// provider keeps the original managed-inference/ prefix of existing agent selections.
const (
	ChatProvider      = "managed-inference"
	ResponsesProvider = "inference-codex"
	MessagesProvider  = "inference-claude"
)

var providers = map[string]string{APIChat: ChatProvider, APIResponses: ResponsesProvider, APIMessages: MessagesProvider}

// Provider returns the OpenCode provider of a wire API; unknown or empty means chat.
func Provider(api string) string {
	if p, ok := providers[api]; ok {
		return p
	}
	return ChatProvider
}

// WireAPI returns a model's wire API, defaulting to chat.
func (m Model) WireAPI() string {
	if m.API == "" {
		return APIChat
	}
	return m.API
}

// SplitModel separates a known provider prefix from a selected model ID. Without a known
// prefix the API is empty and the whole ID is the gateway model name.
func SplitModel(id string) (api, name string) {
	for a, p := range providers {
		if rest, ok := strings.CutPrefix(id, p+"/"); ok {
			return a, rest
		}
	}
	return "", id
}

// ModelID is the picker ID of a catalog model.
func ModelID(name string, m Model) string { return Provider(m.WireAPI()) + "/" + name }
