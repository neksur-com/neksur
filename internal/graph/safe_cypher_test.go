// Unit tests for SanitizeCypherLiteral — CR-01 mitigation contract.

package graph

import (
	"errors"
	"strings"
	"testing"
)

func TestSanitizeCypherLiteral_AcceptsSafeInputs(t *testing.T) {
	cases := []string{
		"",
		"hello",
		"snapshot_id_42",
		"s3://bucket/path/file.parquet",
		"polaris://prod-warehouse/db.table",
		"alice@example.com",
		"spiffe://neksur/tenant-uuid/user",
		"urn:neksur:user:42",
		"prod.sales.orders",
		"abc-DEF-123_xyz",
		"a1b2c3d4-e5f6-4789-8abc-def012345678", // tenant UUID
		"sql_server://db:1433/schema?ssl=true&pool=10",
		"path with spaces is allowed",
		"namespace://table[partition]",
		"prefix*wildcard",
	}
	for _, c := range cases {
		out, err := SanitizeCypherLiteral(c)
		if err != nil {
			t.Errorf("SanitizeCypherLiteral(%q): unexpected error: %v", c, err)
		}
		if out != c {
			t.Errorf("SanitizeCypherLiteral(%q): output = %q, want unchanged", c, out)
		}
		if !IsSafeCypherLiteral(c) {
			t.Errorf("IsSafeCypherLiteral(%q): expected true", c)
		}
	}
}

func TestSanitizeCypherLiteral_RejectsCypherInjection(t *testing.T) {
	// Canonical CR-01 attack payload: a Dataset URI that closes the
	// inner Cypher string literal and opens a tenant-rewriting clause.
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "single_quote_breaks_cypher_string",
			input: "polaris://x'); MATCH (n) SET n.tenant_id='evil';--",
		},
		{
			name:  "single_quote_alone",
			input: "ev'il",
		},
		{
			name:  "double_quote",
			input: `ev"il`,
		},
		{
			name:  "backslash_attempted_escape",
			input: `ev\il`,
		},
		{
			name:  "dollar_sign_could_close_dollar_quote",
			input: "ev$$il",
		},
		{
			name:  "curly_braces_could_inject_property_map",
			input: "x', tenant_id: 'other-tenant'}",
		},
		{
			name:  "semicolon_could_chain_statements",
			input: "x; MATCH (n) DELETE n",
		},
		{
			name:  "newline_breaks_dollar_quote_block",
			input: "x\n; DROP",
		},
		{
			name:  "carriage_return",
			input: "x\r;DROP",
		},
		{
			name:  "null_byte",
			input: "x\x00drop",
		},
		{
			name:  "tab",
			input: "x\tdrop",
		},
		{
			name:  "backtick_could_quote_identifier",
			input: "x`drop`",
		},
		{
			name:  "non_ascii_unicode",
			input: "snäpshot",
		},
		{
			name:  "angle_brackets",
			input: "x<script>",
		},
		{
			name:  "exclamation",
			input: "x!y",
		},
		{
			name:  "pipe",
			input: "x|y",
		},
		{
			name:  "caret",
			input: "x^y",
		},
		{
			name:  "hash",
			input: "x#y",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := SanitizeCypherLiteral(tc.input)
			if err == nil {
				t.Errorf("SanitizeCypherLiteral(%q): expected error, got nil (output=%q)", tc.input, out)
				return
			}
			if !errors.Is(err, ErrCypherUnsafeLiteral) {
				t.Errorf("SanitizeCypherLiteral(%q): got error %v, want ErrCypherUnsafeLiteral wrap", tc.input, err)
			}
			if out != "" {
				t.Errorf("SanitizeCypherLiteral(%q): on error want \"\", got %q", tc.input, out)
			}
			if IsSafeCypherLiteral(tc.input) {
				t.Errorf("IsSafeCypherLiteral(%q): expected false", tc.input)
			}
		})
	}
}

func TestSanitizeCypherLiteral_ErrorReportsOffsetAndByte(t *testing.T) {
	_, err := SanitizeCypherLiteral("abc'def")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "offset 3") {
		t.Errorf("error %q should report offset 3 of the unsafe char", msg)
	}
	if !strings.Contains(msg, "0x27") {
		t.Errorf("error %q should report byte 0x27 (single quote)", msg)
	}
}

func TestMustSanitizeCypherLiteral_PanicsOnUnsafe(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on unsafe input, got nil")
		}
	}()
	_ = MustSanitizeCypherLiteral("evil'payload")
}

func TestMustSanitizeCypherLiteral_PassesSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unexpected panic on safe input: %v", r)
		}
	}()
	out := MustSanitizeCypherLiteral("safe-input_42")
	if out != "safe-input_42" {
		t.Errorf("output = %q, want unchanged", out)
	}
}
