package baseurl

import (
	"net/url"
	"testing"
)

func TestPath(t *testing.T) {
	for _, s := range []string{"https://h", "https://h/", "https://h/multica", "https://h/a/b-c_d.e", "https://h/a/"} {
		u, _ := url.Parse(s)
		if !Path(u) {
			t.Errorf("rejected %s", s)
		}
	}
	for _, s := range []string{"https://h//", "https://h/a//", "https://h//a", "https://h/a/../b", "https://h/a/./b", "https://h/a%2Fb", "https://h/a%25b", "https://h/a;b", "https://h/a\\b", "https://h/a//b"} {
		u, err := url.Parse(s)
		if err == nil && Path(u) {
			t.Errorf("accepted %s", s)
		}
	}
	u, _ := url.Parse("https://h:8443/multica/")
	if Join(u) != "https://h:8443/multica" {
		t.Fatal(Join(u))
	}
}
