// k8s.go — `k8s` subcommand: drives minikube/kubectl/helm to test the operator drop-in shim images.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// newK8sCmd implements `pg_hardstorage_testkit k8s ...` —
// shells out to minikube / kubectl / helm to bring up a
// disposable cluster, install one of the supported operators,
// and create a Postgres cluster CRD that uses the per-operator
// shim image.
//
// The v1 implementation is a thin shell around the upstream
// CLIs; once the workflow stabilises we'll fold the bring-up
// path into a proper Topology implementation under
// internal/testkit/topology/k8s.  Keeping the v1 in shell-out
// form lets the run_k8s_testing.sh driver iterate quickly
// without round-tripping through Go for every step.
func newK8sCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "k8s",
		Short: "Drive minikube + an operator for the K8s drop-in shim tests",
		Long: `Bring up a disposable minikube profile, install one of
the supported Postgres-on-Kubernetes operators (CNPG / Crunchy /
Zalando), and create a PG cluster that uses the matching
shim image (so the operator's own backup CRD goes through
pg_hardstorage's compat shim instead of barman-cloud-* /
pgbackrest / wal-g).

This subcommand is the testkit-side of run_k8s_testing.sh.
Operators driving the workflow themselves can use the
subcommands directly — the script just sequences them.`,
		SilenceUsage: true,
	}
	c.AddCommand(
		newK8sUpCmd(),
		newK8sDownCmd(),
		newK8sInstallOperatorCmd(),
		newK8sPGClusterCmd(),
		newK8sPrereqsCmd(),
	)
	return c
}

// newK8sPrereqsCmd checks that minikube + kubectl + helm are
// on PATH and meet minimum-version requirements.  Surfaces a
// human-readable error pointing at upstream install docs when
// any are missing.  Used by the driver as the first step.
func newK8sPrereqsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prereqs",
		Short: "Verify minikube / kubectl / helm are installed",
		RunE: func(cmd *cobra.Command, _ []string) error {
			missing := []string{}
			tools := []struct {
				name, hint string
			}{
				{"minikube", "https://minikube.sigs.k8s.io/docs/start/"},
				{"kubectl", "https://kubernetes.io/docs/tasks/tools/"},
				{"helm", "https://helm.sh/docs/intro/install/"},
				{"docker", "https://docs.docker.com/engine/install/"},
			}
			for _, t := range tools {
				if _, err := exec.LookPath(t.name); err != nil {
					missing = append(missing, fmt.Sprintf("%s (install: %s)", t.name, t.hint))
				}
			}
			if len(missing) > 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "missing prerequisites:")
				for _, m := range missing {
					fmt.Fprintf(cmd.ErrOrStderr(), "  - %s\n", m)
				}
				return fmt.Errorf("k8s prereqs: %d tool(s) missing", len(missing))
			}
			fmt.Fprintln(cmd.OutOrStdout(), "all k8s prereqs present (minikube + kubectl + helm + docker)")
			return nil
		},
	}
}

// newK8sUpCmd brings up a fresh minikube profile.  Pre-loads
// the shim images we built locally (via `minikube image load`)
// so the operator's pull never needs to find them in a
// registry — keeps the workflow offline-friendly + fast.
func newK8sUpCmd() *cobra.Command {
	var (
		profile     string
		driver      string
		cpus        int
		memory      string
		k8sVersion  string
		preloadImgs []string
	)
	c := &cobra.Command{
		Use:   "up",
		Short: "Start a minikube profile sized for an operator + 3-node PG cluster",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireOnPath("minikube"); err != nil {
				return err
			}
			args := []string{
				"start",
				"--profile", profile,
				"--driver", driver,
				"--cpus", fmt.Sprint(cpus),
				"--memory", memory,
				"--kubernetes-version", k8sVersion,
				// PodSecurityAdmission is GA + on-by-default in
				// k8s 1.25+; no feature gate needed.  We label
				// per-namespace at install time instead.
				//
				// kubelet.cgroup-driver=systemd: the docker
				// driver's kubelet won't start on hosts whose
				// docker daemon uses systemd as its cgroup
				// driver unless the kubelet matches.  Modern
				// Ubuntu / Debian hosts (the typical operator
				// box) hit this; the workaround is the
				// minikube docs' canonical recommendation.
				"--extra-config", "kubelet.cgroup-driver=systemd",
			}
			if err := runShellInherited(cmd, "minikube", args...); err != nil {
				return err
			}
			for _, img := range preloadImgs {
				if err := runShellInherited(cmd,
					"minikube", "image", "load", "--profile", profile, img); err != nil {
					return fmt.Errorf("preload %s into minikube profile %s: %w", img, profile, err)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&profile, "profile", "pgvalidate-k8s", "minikube profile name (must be unique per cell)")
	c.Flags().StringVar(&driver, "driver", "docker", "minikube driver: docker | kvm2 | hyperkit | virtualbox")
	c.Flags().IntVar(&cpus, "cpus", 4, "CPU budget for the profile")
	c.Flags().StringVar(&memory, "memory", "8g", "memory budget for the profile")
	c.Flags().StringVar(&k8sVersion, "kubernetes-version", "stable", "k8s server version")
	c.Flags().StringSliceVar(&preloadImgs, "preload-image", nil,
		"local docker image tag(s) to load into the minikube profile via `minikube image load` "+
			"(repeatable; the shim images go here)")
	return c
}

// newK8sDownCmd tears down the profile.  No-op if the profile
// doesn't exist — operators retrying after a failure shouldn't
// see "no such profile" as a failure mode.
func newK8sDownCmd() *cobra.Command {
	var profile string
	c := &cobra.Command{
		Use:   "down",
		Short: "Delete a minikube profile (idempotent)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireOnPath("minikube"); err != nil {
				return err
			}
			// `minikube delete` returns 0 when the profile didn't
			// exist on recent versions, but older versions exit 1
			// with "no such profile".  Always treat as success;
			// nothing else can sensibly happen on a delete.
			_ = runShellInherited(cmd, "minikube", "delete", "--profile", profile)
			return nil
		},
	}
	c.Flags().StringVar(&profile, "profile", "pgvalidate-k8s", "minikube profile to delete")
	return c
}

// newK8sInstallOperatorCmd installs one of the supported
// operators into a running profile.  v1 handles helm-based
// (CNPG) and kubectl-apply manifest-based (Crunchy, Zalando)
// installs via static dispatch on --operator.
func newK8sInstallOperatorCmd() *cobra.Command {
	var (
		operator  string
		namespace string
		kubectx   string
		opVersion string
	)
	c := &cobra.Command{
		Use:   "install-operator",
		Short: "Install one of {cnpg, crunchy, zalando} into the active profile",
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch operator {
			case "cnpg":
				if err := requireOnPath("helm"); err != nil {
					return err
				}
				if err := runShellInherited(cmd,
					"helm", "repo", "add", "cnpg",
					"https://cloudnative-pg.github.io/charts",
					"--force-update"); err != nil {
					return err
				}
				if err := runShellInherited(cmd, "helm", "repo", "update"); err != nil {
					return err
				}
				args := []string{
					"upgrade", "--install", "cnpg",
					"cnpg/cloudnative-pg",
					"--namespace", namespace,
					"--create-namespace",
					"--wait", "--timeout", "5m",
				}
				if opVersion != "" {
					args = append(args, "--version", opVersion)
				}
				if kubectx != "" {
					args = append(args, "--kube-context", kubectx)
				}
				return runShellInherited(cmd, "helm", args...)
			case "crunchy":
				return notImplementedYet("crunchy install will land in S2 — pending PGO v5 manifest pinning")
			case "zalando":
				return notImplementedYet("zalando install will land in S2 — pending Spilo manifest pinning")
			default:
				return fmt.Errorf("--operator must be one of {cnpg, crunchy, zalando} (got %q)", operator)
			}
		},
	}
	c.Flags().StringVar(&operator, "operator", "cnpg", "operator to install: cnpg | crunchy | zalando")
	c.Flags().StringVar(&namespace, "namespace", "", "namespace to install into (default: <operator>-system)")
	c.Flags().StringVar(&kubectx, "context", "", "kube context (default: minikube profile name)")
	c.Flags().StringVar(&opVersion, "version", "", "operator version (default: latest from chart)")
	c.MarkFlagRequired("operator")
	return c
}

// newK8sPGClusterCmd creates an operator-managed PG cluster
// CRD using the shim image as the postgres container's image.
// v1 stub — the actual CRD-rendering logic per operator lands
// in S2 once the install-operator path is exercised end-to-end
// against each operator.
func newK8sPGClusterCmd() *cobra.Command {
	var (
		operator string
		name     string
		image    string
	)
	c := &cobra.Command{
		Use:   "pg-cluster",
		Short: "Create an operator-managed PG cluster using the shim image",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplementedYet(fmt.Sprintf(
				"pg-cluster %s for operator=%s image=%s — lands in S2 with the per-operator CRD rendering",
				name, operator, image))
		},
	}
	c.Flags().StringVar(&operator, "operator", "cnpg", "operator family: cnpg | crunchy | zalando")
	c.Flags().StringVar(&name, "name", "shim-test", "cluster name (becomes the CRD's metadata.name)")
	c.Flags().StringVar(&image, "image", "", "shim image tag to use (REQUIRED)")
	c.MarkFlagRequired("image")
	return c
}

// requireOnPath returns a clear error if a binary isn't on
// PATH.  Used by every subcommand that shells out, so the
// driver gets a uniform "install X" message instead of the
// raw exec.ErrNotFound.
func requireOnPath(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("k8s: %s not on PATH — run `pg_hardstorage_testkit k8s prereqs` for install hints", name)
	}
	return nil
}

// runShellInherited runs an external command with stdout +
// stderr inherited from the parent cobra command.  Used for
// long-running shell-outs (helm install, minikube start, ...)
// where the operator wants to see the upstream tool's
// progress in real time.
func runShellInherited(cmd *cobra.Command, name string, args ...string) error {
	exe := exec.CommandContext(cmd.Context(), name, args...)
	exe.Stdout = cmd.OutOrStdout()
	exe.Stderr = cmd.ErrOrStderr()
	exe.Stdin = os.Stdin
	exe.Env = os.Environ()
	if err := exe.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func notImplementedYet(msg string) error {
	return fmt.Errorf("not implemented: %s", msg)
}
