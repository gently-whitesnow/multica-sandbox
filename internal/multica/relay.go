package multica

import "strings"

// RelayPrefix keeps the upstream CLI's task-token check; the suffix is longer than a real mat_ token.
const RelayPrefix = "mat_relay_"

// CLIDir is where the controller mounts the CLI artifact (deploy/multica-cli.Dockerfile);
// its bin directory leads PATH, so image copies cannot shadow it.
const CLIDir = "/opt/multica-sandbox/multica"

// RelayPath confines credential translation to the configured Multica API.
// Multica authorizes every route and method using the native task token.
func RelayPath(path string) bool { return strings.HasPrefix(path, "/api/") }
