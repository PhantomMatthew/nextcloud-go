package auth

import "testing"

func TestParseBasicHeader(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		wantUser string
		wantPass string
		wantOK   bool
	}{
		{"empty", "", "", "", false},
		{"missing prefix", "dXNlcjpwYXNz", "", "", false},
		{"wrong scheme", "Bearer abc", "", "", false},
		{"valid", "Basic " + b64("admin:secret"), "admin", "secret", true},
		{"valid lowercase scheme", "basic " + b64("admin:secret"), "admin", "secret", true},
		{"colon in password", "Basic " + b64("admin:se:cret"), "admin", "se:cret", true},
		{"empty password", "Basic " + b64("admin:"), "admin", "", true},
		{"no colon", "Basic " + b64("adminonly"), "", "", false},
		{"invalid base64", "Basic !!!notb64!!!", "", "", false},
		{"trailing whitespace", "Basic " + b64("admin:secret") + "  ", "admin", "secret", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, p, ok := ParseBasicHeader(tt.header)
			if ok != tt.wantOK || u != tt.wantUser || p != tt.wantPass {
				t.Fatalf("ParseBasicHeader(%q)=(%q,%q,%v); want (%q,%q,%v)",
					tt.header, u, p, ok, tt.wantUser, tt.wantPass, tt.wantOK)
			}
		})
	}
}

func b64(s string) string {
	return encStd(s)
}
