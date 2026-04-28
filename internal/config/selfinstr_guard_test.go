package config

import "testing"

func TestHostFromEndpoint(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"localhost:4317", "localhost"},
		{"127.0.0.1:4317", "127.0.0.1"},
		{"[::1]:4317", "::1"},
		{"http://localhost:4317", "localhost"},
		{"https://collector.example.com:4317", "collector.example.com"},
		{"otelcollector:4317", "otelcollector"},
		{"localhost", "localhost"},
		{"   ", ""},
		{"", ""},
		{"http://localhost:4317/v1/traces", "localhost"},
	}
	for _, tc := range cases {
		if got := hostFromEndpoint(tc.in); got != tc.want {
			t.Errorf("hostFromEndpoint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	loopback := []string{"", "localhost", "127.0.0.1", "127.1.2.3", "::1"}
	for _, h := range loopback {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
	}
	notLoopback := []string{"otelcollector", "10.0.0.1", "collector.example.com", "192.168.1.1"}
	for _, h := range notLoopback {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
	}
}

func TestHasService(t *testing.T) {
	t.Parallel()
	cases := []struct {
		list, service string
		want          bool
	}{
		{"", "otelcontext", false},
		{"otelcontext", "otelcontext", true},
		{"a,b,otelcontext,c", "otelcontext", true},
		{"a, otelcontext , c", "otelcontext", true},
		{"a,b,c", "otelcontext", false},
		{"otelcontextual", "otelcontext", false},
	}
	for _, tc := range cases {
		if got := hasService(tc.list, tc.service); got != tc.want {
			t.Errorf("hasService(%q, %q) = %v, want %v", tc.list, tc.service, got, tc.want)
		}
	}
}

func TestGuardSelfInstrumentation(t *testing.T) {
	t.Run("NoOpWhenEndpointEmpty", func(t *testing.T) {
		c := &Config{IngestExcludedServices: "foo"}
		c.GuardSelfInstrumentation()
		if c.IngestExcludedServices != "foo" {
			t.Fatalf("modified excluded list when endpoint empty: %q", c.IngestExcludedServices)
		}
	})

	t.Run("AutoAddsWhenLoopback", func(t *testing.T) {
		c := &Config{OTelExporterEndpoint: "localhost:4317"}
		c.GuardSelfInstrumentation()
		if !hasService(c.IngestExcludedServices, SelfServiceName) {
			t.Fatalf("self service not auto-added: %q", c.IngestExcludedServices)
		}
	})

	t.Run("PrependsToExistingList", func(t *testing.T) {
		c := &Config{
			OTelExporterEndpoint:   "127.0.0.1:4317",
			IngestExcludedServices: "noisy-svc",
		}
		c.GuardSelfInstrumentation()
		want := SelfServiceName + ",noisy-svc"
		if c.IngestExcludedServices != want {
			t.Fatalf("got %q, want %q", c.IngestExcludedServices, want)
		}
	})

	t.Run("IdempotentWhenAlreadyExcluded", func(t *testing.T) {
		c := &Config{
			OTelExporterEndpoint:   "[::1]:4317",
			IngestExcludedServices: "a," + SelfServiceName + ",b",
		}
		before := c.IngestExcludedServices
		c.GuardSelfInstrumentation()
		if c.IngestExcludedServices != before {
			t.Fatalf("guard mutated already-excluded list: %q -> %q", before, c.IngestExcludedServices)
		}
	})

	t.Run("NoOpForRemoteEndpoint", func(t *testing.T) {
		c := &Config{
			OTelExporterEndpoint:   "collector.example.com:4317",
			IngestExcludedServices: "foo",
		}
		c.GuardSelfInstrumentation()
		if hasService(c.IngestExcludedServices, SelfServiceName) {
			t.Fatalf("guard fired on remote endpoint: %q", c.IngestExcludedServices)
		}
	})

	t.Run("NilSafe", func(t *testing.T) {
		var c *Config
		c.GuardSelfInstrumentation()
	})
}
