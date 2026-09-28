package config

import (
	"reflect"
	"testing"
)

// TestServerConfigHasNoRegisterPassword proves the v1 shared register-password
// secret was removed from server configuration at the auth cutover.
func TestServerConfigHasNoRegisterPassword(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	if _, ok := typ.FieldByName("RegisterPassword"); ok {
		t.Fatal("Config must not have a RegisterPassword field after the auth cutover")
	}
}

// TestLoadIgnoresRegisterPasswordEnv proves REGISTER_PASSWORD has no effect on
// the loaded configuration.
func TestLoadIgnoresRegisterPasswordEnv(t *testing.T) {
	t.Setenv("REGISTER_PASSWORD", "should-be-ignored")
	cfg := Load()
	v := reflect.ValueOf(*cfg)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).Kind() == reflect.String && v.Field(i).String() == "should-be-ignored" {
			t.Fatalf("field %s picked up REGISTER_PASSWORD", v.Type().Field(i).Name)
		}
	}
}

func TestDerivePreviewPublicURL(t *testing.T) {
	tests := []struct {
		name     string
		public   string
		explicit string
		want     string
	}{
		{name: "localhost default splits onto loopback ip", public: "http://localhost:8080", want: "http://127.0.0.1:8080"},
		{name: "localhost without port", public: "http://localhost", want: "http://127.0.0.1"},
		{name: "explicit preview origin wins", public: "http://localhost:8080", explicit: "https://preview.example", want: "https://preview.example"},
		{name: "off disables the split", public: "http://localhost:8080", explicit: "off", want: ""},
		{name: "same-origin disables the split", public: "http://localhost:8080", explicit: "same-origin", want: ""},
		{name: "non-loopback stays same-origin until configured", public: "https://cowork.example", want: ""},
		{name: "ipv4 loopback is not auto-split", public: "http://127.0.0.1:8080", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := derivePreviewPublicURL(tt.public, tt.explicit); got != tt.want {
				t.Fatalf("derivePreviewPublicURL(%q, %q) = %q; want %q", tt.public, tt.explicit, got, tt.want)
			}
		})
	}
}

func TestLoadUsesPreviewPublicURLEnv(t *testing.T) {
	t.Setenv("PUBLIC_URL", "http://localhost:9090")
	t.Setenv("PREVIEW_PUBLIC_URL", "http://127.0.0.1:9090")
	cfg := Load()
	if cfg.PublicURL != "http://localhost:9090" || cfg.PreviewPublicURL != "http://127.0.0.1:9090" {
		t.Fatalf("Load() = %+v", cfg)
	}
}
