// llm_mcp.go — 'llm --mcp-server' mode: JSON-RPC MCP server over stdin/stdout for AI clients.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/llm/mcp"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/output"
	"github.com/cybertec-postgresql/pg_hardstorage/internal/version"
)

// runMCPServer drives `pg_hardstorage llm --mcp-server`.
// Reads JSON-RPC from stdin, writes responses to stdout.  An
// MCP-aware client (Claude Desktop, Cursor, Zed, Goose, Cline)
// invokes the binary with this flag and speaks the protocol
// over the resulting subprocess.
//
// Tool surface + builtin skills come from the same loader the
// chat path uses, so an operator who has dropped a skill
// override at /etc/pg_hardstorage/skills/ sees that skill in
// their MCP client's prompt list.
func runMCPServer(cmd *cobra.Command) error {
	skillSet, err := loadSkillSet()
	if err != nil {
		return output.NewError("llm.skill_load_failed",
			"llm --mcp-server: load skills: "+err.Error()).Wrap(err)
	}
	toolReg, _, _ := buildLiveToolRegistry(nil, cmd.Root())

	srv := &mcp.Server{
		In:     cmd.InOrStdin(),
		Out:    cmd.OutOrStdout(),
		Err:    cmd.ErrOrStderr(),
		Tools:  toolReg,
		Skills: skillSet,
		Info: mcp.ServerInfo{
			Name:    "pg_hardstorage",
			Version: version.Version,
		},
	}
	if err := srv.Run(cmd.Context()); err != nil {
		return output.NewError("llm.mcp_server_failed",
			"llm --mcp-server: "+err.Error()).Wrap(err)
	}
	return nil
}
