package ingest

import (
	"errors"
	"strings"
	"testing"
)

func TestValidDeliveryID(t *testing.T) {
	for _, valid := range []string{
		"delivery-0001",
		"01234567",
		"Aa0._-Zz",
		strings.Repeat("d", 128),
	} {
		if err := ValidDeliveryID(valid); err != nil {
			t.Fatalf("ValidDeliveryID(%q) = %v, want nil", valid, err)
		}
	}
	for _, invalid := range []string{
		"short",
		strings.Repeat("d", 129),
		"delivery 0001",
		"delivery/0001",
		"delivery:0001",
		"delivery\n0001",
	} {
		if err := ValidDeliveryID(invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ValidDeliveryID(%q) = %v, want ErrInvalid", invalid, err)
		}
	}
}

func TestSanitizeText(t *testing.T) {
	text, err := SanitizeText("line one\r\nline\u0000two\n\n", 1000)
	if err != nil || text != "line one\nlinetwo" {
		t.Fatalf("SanitizeText = %q err=%v", text, err)
	}
	if text, err = SanitizeText("  plain  ", 1000); err != nil || text != "plain" {
		t.Fatalf("trim = %q err=%v", text, err)
	}
	if _, err = SanitizeText(strings.Repeat("x", 1001), 1000); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too long error = %v, want ErrInvalid", err)
	}
	if _, err = SanitizeText(string([]byte{0xff, 0xfe}), 1000); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid UTF-8 error = %v, want ErrInvalid", err)
	}
	if text, err = SanitizeText("\n\n", 1000); err != nil || text != "" {
		t.Fatalf("blank = %q err=%v", text, err)
	}
}

func TestValidateURL(t *testing.T) {
	for _, valid := range []string{
		"",
		"https://example.com/reports/1?state=open",
		"http://127.0.0.1:8080/scan/2",
	} {
		if err := ValidateURL("reportUrl", valid); err != nil {
			t.Fatalf("ValidateURL(%q) = %v, want nil", valid, err)
		}
	}
	for _, invalid := range []string{
		"ftp://example.com/x",
		"https://user:pass@example.com/x",
		"https://example.com/x#frag",
		"/relative/path",
		"not a url",
		"https:///no-host",
		"https://example.com/" + strings.Repeat("a", 2048),
	} {
		if err := ValidateURL("reportUrl", invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ValidateURL(%q) = %v, want ErrInvalid", invalid, err)
		}
	}
}

func TestBoundedSingleLine(t *testing.T) {
	for _, tc := range []struct {
		name         string
		value        string
		max          int
		emptyIsValid bool
		want         string
		valid        bool
	}{
		{"trims", "  scanner  ", 64, false, "scanner", true},
		{"optional empty", "", 64, true, "", true},
		{"required empty", "  ", 64, false, "", false},
		{"too long", strings.Repeat("s", 65), 64, false, "", false},
		{"newline", "a\nb", 64, false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := BoundedSingleLine("field", tc.value, tc.max, tc.emptyIsValid)
			if tc.valid {
				if err != nil || value != tc.want {
					t.Fatalf("value = %q err = %v", value, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}
