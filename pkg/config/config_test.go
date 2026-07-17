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
