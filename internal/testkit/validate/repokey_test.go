package validate

import "testing"

// Cells on the bind-mounted default repository share it; cells with a
// sink of their own do not, even when two sinks' URLs coincide (the GCS
// fake's URL carries no endpoint — the difference is in the agent env).
func TestRepoKeyDistinguishesRepositories(t *testing.T) {
	file := func() *DockerCellRuntime { return &DockerCellRuntime{RepoURL: "file:///var/lib/pg_hardstorage/repo"} }
	if file().RepoKey() != file().RepoKey() {
		t.Error("two cells on the default repository must share a key")
	}
	a := &DockerCellRuntime{RepoURL: "gcs://b", sinkAgentEnv: map[string]string{"STORAGE_EMULATOR_HOST": "127.0.0.1:1"}}
	b := &DockerCellRuntime{RepoURL: "gcs://b", sinkAgentEnv: map[string]string{"STORAGE_EMULATOR_HOST": "127.0.0.1:2"}}
	if a.RepoKey() == b.RepoKey() {
		t.Error("two sinks that differ only in agent env are two repositories")
	}
}
