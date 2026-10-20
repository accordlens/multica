package passkey

import "testing"

func TestConfigFromAppURL(t *testing.T) {
	for _, tt := range []struct {
		name, appURL, override string
		wantRPID, wantOrigin   string
		wantErr                bool
	}{
		{name: "tailscale https", appURL: "https://multica.tail1234.ts.net", wantRPID: "multica.tail1234.ts.net", wantOrigin: "https://multica.tail1234.ts.net"},
		{name: "trailing slash and port", appURL: "https://Multica.Example:8443/", wantRPID: "multica.example", wantOrigin: "https://Multica.Example:8443"},
		{name: "parent domain override", appURL: "https://app.example.com", override: "example.com", wantRPID: "example.com", wantOrigin: "https://app.example.com"},
		{name: "localhost dev", appURL: "http://localhost:3000", wantRPID: "localhost", wantOrigin: "http://localhost:3000"},
		{name: "plain http host", appURL: "http://100.64.0.1:3000", wantErr: true},
		{name: "plain http name", appURL: "http://multica.tail1234.ts.net", wantErr: true},
		{name: "unrelated override", appURL: "https://app.example.com", override: "evil.com", wantErr: true},
		{name: "suffix but not parent", appURL: "https://app.example.com", override: "le.com", wantErr: true},
		{name: "path", appURL: "https://example.com/multica", wantErr: true},
		{name: "empty", appURL: "", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConfigFromAppURL(tt.appURL, tt.override)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("got %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.RPID != tt.wantRPID || got.Origin != tt.wantOrigin {
				t.Fatalf("got %+v, want rp %q origin %q", got, tt.wantRPID, tt.wantOrigin)
			}
			if _, err := got.WebAuthn(); err != nil {
				t.Fatalf("relying party: %v", err)
			}
		})
	}
}

func TestEnrollmentURLEscapesToken(t *testing.T) {
	if got := EnrollmentURL("https://m.example/", "a+b/c"); got != "https://m.example/login?enroll=a%2Bb%2Fc" {
		t.Fatalf("got %q", got)
	}
}
