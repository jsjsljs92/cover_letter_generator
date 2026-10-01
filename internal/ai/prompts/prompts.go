// Package prompts holds the model prompts for each workflow stage.
//
// They live in .tmpl files rather than Go string literals so wording can be
// tuned without touching control flow, and so the prompts can be read as
// documents in review. The templates are compiled once at init, which turns a
// syntax error into a startup failure instead of a per-request panic.
package prompts

import (
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed *.tmpl
var files embed.FS

var registry = map[string]*template.Template{}

// Stage prompt names.
const (
	AnalyzeJob     = "analyze_job"
	AnalyzeResume  = "analyze_resume"
	MatchCandidate = "match_candidate"
	GenerateLetter = "generate_letter"
	ValidateLetter = "validate_letter"
)

func init() {
	// "join" renders a []string with a separator, which keeps the templates
	// readable instead of nesting range blocks around every list.
	funcs := template.FuncMap{
		"join": func(items []string, sep string) string { return strings.Join(items, sep) },
	}

	for _, name := range []string{AnalyzeJob, AnalyzeResume, MatchCandidate, GenerateLetter, ValidateLetter} {
		body, err := files.ReadFile(name + ".tmpl")
		if err != nil {
			panic(fmt.Sprintf("prompts: missing template %q: %v", name, err))
		}
		parsed, err := template.New(name).Funcs(funcs).Option("missingkey=error").Parse(string(body))
		if err != nil {
			panic(fmt.Sprintf("prompts: parse template %q: %v", name, err))
		}
		registry[name] = parsed
	}
}

// System returns the system instruction for a stage.
func System(stage string) (string, error) {
	tpl, err := lookup(stage)
	if err != nil {
		return "", err
	}
	return render(tpl, "system", nil)
}

// User returns the user prompt for a stage, rendered with data.
func User(stage string, data any) (string, error) {
	tpl, err := lookup(stage)
	if err != nil {
		return "", err
	}
	return render(tpl, "user", data)
}

func lookup(stage string) (*template.Template, error) {
	tpl, ok := registry[stage]
	if !ok {
		return nil, fmt.Errorf("prompts: unknown stage %q", stage)
	}
	return tpl, nil
}

func render(tpl *template.Template, section string, data any) (string, error) {
	var sb strings.Builder
	if err := tpl.ExecuteTemplate(&sb, section, data); err != nil {
		return "", fmt.Errorf("prompts: render %q/%s: %w", tpl.Name(), section, err)
	}
	return strings.TrimSpace(sb.String()), nil
}
