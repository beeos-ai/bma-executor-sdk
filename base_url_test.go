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
