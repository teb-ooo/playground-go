package assistant

import (
	"fmt"
	"strings"

	"github.com/teb-ooo/factory-go/openapimcp"
)

// FirstParagraph returns the first prose paragraph of a SPEC.md document:
// headings, blank lines and front matter are skipped, and the paragraph's
// lines are joined with spaces.
func FirstParagraph(spec string) string {
	lines := strings.Split(strings.ReplaceAll(spec, "\r\n", "\n"), "\n")
	i := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" { // front matter
		for j := 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "---" {
				i = j + 1
				break
			}
		}
	}
	var para []string
	for ; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		switch {
		case l == "":
			if len(para) > 0 {
				return strings.Join(para, " ")
			}
		case strings.HasPrefix(l, "#"):
			if len(para) > 0 {
				return strings.Join(para, " ")
			}
		default:
			para = append(para, l)
		}
	}
	return strings.Join(para, " ")
}

// buildSystemPrompt is the SPEC.md first paragraph followed by one line per
// operation (its summary), then standing rules.
func buildSystemPrompt(appName, spec string, ts *openapimcp.Toolset) string {
	var b strings.Builder
	if appName == "" {
		appName = "this app"
	}
	fmt.Fprintf(&b, "You are the assistant built into %s.", appName)
	if p := FirstParagraph(spec); p != "" {
		fmt.Fprintf(&b, " About the app: %s", p)
	}
	b.WriteString("\n\nYou act for the signed-in user, with exactly their permissions, by calling tools. Each tool is one operation of this app's API:\n")
	for _, t := range ts.Tools() {
		fmt.Fprintf(&b, "- %s: %s\n", t.Name, t.Summary)
	}
	b.WriteString("\nRules:\n")
	b.WriteString("- Use the tools to read or change data; never invent records or values you have not seen.\n")
	b.WriteString("- Ask before deleting or overwriting anything the user did not explicitly name.\n")
	b.WriteString("- A tool error starts with the HTTP status. Say plainly what failed and what the user can do next; do not retry blindly.\n")
	b.WriteString("- Keep answers short and plain. No markdown headings.\n")
	return b.String()
}
