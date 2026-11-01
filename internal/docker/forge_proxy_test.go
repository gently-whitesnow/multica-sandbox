package docker

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestForgeProxyRouting(t *testing.T) {
	if os.Getenv("SANDBOX_FORGE_PROXY_PROBE") == "1" {
		for address, proxied := range map[string]bool{
			"https://forge.example.test/api/graphql":            false,
			"http://forge.example.test/team/repo.git/info/refs": true,
			"https://public.example.test/api":                   true,
			"http://multica-relay:8091/api":                     false,
			"http://inference-relay:8092/v1":                    false,
			"https://upper.example.test":                        false,
			"https://lower.example.test":                        false,
		} {
			request, _ := http.NewRequest(http.MethodGet, address, nil)
			proxy, err := http.ProxyFromEnvironment(request)
			if err != nil || (proxy != nil) != proxied {
				t.Errorf("%s: proxy=%v error=%v, want proxied=%t", address, proxy, err, proxied)
			}
		}
		return
	}
	// net/http caches proxy environment once; a child exercises the real parser.
	child := exec.Command(os.Args[0], "-test.run=^TestForgeProxyRouting$")
	child.Env = append(os.Environ(), "SANDBOX_FORGE_PROXY_PROBE=1", "HTTP_PROXY=http://public-proxy:3128", "HTTPS_PROXY=http://public-proxy:3128")
	image := []string{"NO_PROXY=multica-relay,inference-relay,upper.example.test", "no_proxy=lower.example.test"}
	for _, option := range forgeProxyBypass(image, []string{"forge.example.test"}) {
		child.Env = append(child.Env, strings.TrimPrefix(option, "--env="))
	}
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("proxy routing: %v\n%s", err, out)
	}
	if options := forgeProxyBypass(image, nil); len(options) != 0 {
		t.Fatal("an image without forge APIs must keep its proxy environment")
	}
}
