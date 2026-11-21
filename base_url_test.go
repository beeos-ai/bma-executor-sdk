package bmaexecutor

import "testing"

func TestBaseURLIsTheOrigin(t *testing.T) {
	for _, base := range []string{"https://bma.test", "https://bma.test/", "https://bma.test/api/v1", "https://bma.test/api/v1/"} {
		client, err := NewClient(Options{EnvironmentKey: "k", Executor: ExecutorInfo{Name: "t"}, BaseURL: base})
		if err != nil {
			t.Fatal(err)
		}
		if client.options.BaseURL != "https://bma.test" {
			t.Fatalf("%s: BaseURL = %q", base, client.options.BaseURL)
		}
	}
}

func TestRetiredExecutorHostsMoveToTheExecutorHost(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.bma.beeos.ai":              "https://executor.beeos.ai",
		"https://api.bma.beeos.ai/api/v1/":      "https://executor.beeos.ai",
		"https://API.staging.bma.beeos.ai":      "https://executor.staging.beeos.ai",
		"https://api.staging.bma.beeos.ai:443/": "https://executor.staging.beeos.ai:443",
		"https://executor.beeos.ai":             "https://executor.beeos.ai",
		"https://api.beeos.ai":                  "https://api.beeos.ai",
		"":                                      DefaultBaseURL,
	} {
		client, err := NewClient(Options{EnvironmentKey: "k", Executor: ExecutorInfo{Name: "t"}, BaseURL: base})
		if err != nil {
			t.Fatal(err)
		}
		if client.options.BaseURL != want {
			t.Errorf("%q: BaseURL = %q, want %q", base, client.options.BaseURL, want)
		}
	}
	if DefaultBaseURL != "https://executor.beeos.ai" {
		t.Fatalf("DefaultBaseURL = %q", DefaultBaseURL)
	}
	if got, changed := MigrateExecutorURL("https://api.bma.beeos.ai"); !changed || got != "https://executor.beeos.ai" {
		t.Fatalf("MigrateExecutorURL = %q %v", got, changed)
	}
	if _, changed := MigrateExecutorURL("https://executor.beeos.ai/"); changed {
		t.Fatal("an executor host is not migrated")
	}
}
