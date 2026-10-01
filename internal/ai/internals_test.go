package ai

import "testing"

func TestNormalizeLetterBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "strips code fence", in: "```\nHello.\n```", want: "Hello."},
		{name: "collapses blank lines", in: "First.\n\n\n\nSecond.", want: "First.\n\nSecond."},
		{name: "normalises crlf", in: "First.\r\n\r\nSecond.", want: "First.\n\nSecond."},
		{name: "trims surrounding whitespace", in: "  Hello.  ", want: "Hello."},
		{name: "keeps single newlines inside a paragraph", in: "One\ntwo", want: "One\ntwo"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := normalizeLetterBody(tc.in); got != tc.want {
				t.Errorf("normalizeLetterBody(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCheckMissingRequirements(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		missing []string
		want    int
	}{
		{
			name:    "no missing requirements",
			body:    "I have built services in Go for six years.",
			missing: nil,
			want:    0,
		},
		{
			name:    "requirement absent from letter",
			body:    "I have built services in Go for six years.",
			missing: []string{"Hands-on experience operating Kubernetes in production"},
			want:    0,
		},
		{
			name:    "requirement present verbatim",
			body:    "I have hands-on experience operating kubernetes in PRODUCTION for years.",
			missing: []string{"Hands-on experience operating Kubernetes in production"},
			want:    1,
		},
		{
			name:    "punctuation differences do not hide a match",
			body:    "I have hands-on experience operating Kubernetes, in production.",
			missing: []string{"Hands-on experience operating Kubernetes in production"},
			want:    1,
		},
		{
			name:    "very short requirements are ignored",
			body:    "I know Go.",
			missing: []string{"Go"},
			want:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := checkMissingRequirements(GeneratedLetter{Body: tc.body}, tc.missing)
			if len(got) != tc.want {
				t.Errorf("checkMissingRequirements() = %v, want %d findings", got, tc.want)
			}
		})
	}
}

func TestNormalizeForMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{in: "Kubernetes, in Production!", want: "kubernetes in production"},
		{in: "  spaced   out  ", want: "spaced out"},
		{in: "C++/C#", want: "c c"},
	}

	for _, tc := range tests {
		if got := normalizeForMatch(tc.in); got != tc.want {
			t.Errorf("normalizeForMatch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
