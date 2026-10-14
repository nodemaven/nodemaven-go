package nodemaven

import (
	"os"
	"strings"
	"testing"
)

func readme(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// The README quotes real output; a quotation nothing checks is a comment.
func TestReadmeQuotesTheUnknownNameMessage(t *testing.T) {
	text := readme(t)
	start := strings.Index(text, "```text\n")
	if start < 0 {
		t.Fatal("no text block in the README")
	}
	block := text[start+len("```text\n"):]
	block = block[:strings.Index(block, "```")]
	clearGatewayEnv(t)
	_, err := New(WithLogin("u"), WithPassword("p"), WithParam("contry", "us"))
	if err == nil {
		t.Fatal("contry was accepted")
	}
	if strings.Join(strings.Fields(block), " ") != err.Error() {
		t.Fatalf("README quotes\n%s\nthe code says\n%s", block, err)
	}
}

func TestReadmeTableNamesEveryKnownParameter(t *testing.T) {
	text := readme(t)
	provider, err := Load(DefaultProvider)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range provider.KnownParams() {
		if !strings.Contains(text, "| `"+name+"` |") {
			t.Errorf("the parameter table has no row for %q", name)
		}
	}
}

func TestReadmeQuotesTheUsername(t *testing.T) {
	proxy := testProxy(t, WithLogin("your-login"), WithParam("country", "us"), WithParam("sid", "order4417"))
	if !strings.Contains(readme(t), "`"+proxy.Username()+"`") {
		t.Fatalf("the README does not show %s", proxy.Username())
	}
}
