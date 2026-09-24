package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePsql plants a psql that records its argv and PGPASSWORD.
func fakePsql(t *testing.T) (argvFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	envFile = filepath.Join(dir, "env")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\nprintf '%s' \"$PGPASSWORD\" > " + envFile + "\ncat >/dev/null\n"
	if err := os.WriteFile(filepath.Join(dir, "psql"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("PGPASSWORD", "")
	return argvFile, envFile
}

// The password in --pg-connection was handed to psql on its command
// line, where every local user can read it (ps, /proc/<pid>/cmdline).
// It must travel in the environment instead.
func TestDbExtension_PasswordNotOnPsqlArgv(t *testing.T) {
	cases := map[string]string{
		"host=h user=u password='s3 cret' dbname=x": "s3 cret",
		"postgres://u:s3cret@h/db":                  "s3cret",
		"postgres://u@h/db?password=s3cret":         "s3cret",
	}
	for dsn, secret := range cases {
		for _, args := range [][]string{
			{"db", "install-extension", "--pg-connection", dsn, "-o", "json"},
			{"db", "uninstall-extension", "--pg-connection", dsn, "--drop-data", "-o", "json"},
		} {
			argvFile, envFile := fakePsql(t)
			_, errb, exit := runCmd(t, args...)
			if exit != 0 {
				t.Fatalf("%v: exit %d\n%s", args[:2], exit, errb)
			}
			argv, _ := os.ReadFile(argvFile)
			env, _ := os.ReadFile(envFile)
			if strings.Contains(string(argv), secret) {
				t.Errorf("%v %q: password on psql argv:\n%s", args[:2], dsn, argv)
			}
			if string(env) != secret {
				t.Errorf("%v %q: PGPASSWORD = %q, want %q", args[:2], dsn, env, secret)
			}
		}
	}
}
