package agentbuild

import (
	"context"
	"log/slog"

	"github.com/matt-riley/waffle/internal/mcp"
	"github.com/matt-riley/waffle/internal/plugin"
	"github.com/matt-riley/waffle/internal/pluginmcp"
	"github.com/matt-riley/waffle/internal/tool"
)

// wirePluginMCPServer connects one validated portable server entry with the
// most restrictive default posture (#391/#393): every failure — mapping,
// connect, or toolbox — is skipped with a structured log line naming the
// plugin, server, and reason (spec §7.2.2 rule 5 / §11.3), and never fails
// the agent build. Remote plugin servers are refused (the portable surface
// carries no credentials or egress policy; that is the waffle extension
// namespace, #394). Plugin stdio servers are host-executed from the plugin
// root, so docker-mode groups refuse them rather than run untrusted host
// binaries inside a sandboxed tier without a mount story.
func (b *Builder) wirePluginMCPServer(ctx context.Context, boxes *[]tool.Toolbox, closers *[]Cleanup, result plugin.LoadResult, srv plugin.MCPServer, home, sandboxMode string) {
	pluginName := result.Plugin.Manifest.Name
	switch srv.Type {
	case plugin.MCPTypeSSE:
		return // unsupported transport, already skipped at load
	case plugin.MCPTypeStreamableHTTP:
		slog.Warn("plugin remote mcp server refused",
			"plugin", pluginName, "server", srv.Name,
			"reason", "portable mcp.json carries no credentials or egress policy; configure via the waffle extension namespace (#394)")
		return
	case plugin.MCPTypeStdio:
		if sandboxMode == "docker" {
			slog.Warn("plugin stdio mcp server refused",
				"plugin", pluginName, "server", srv.Name,
				"reason", "plugin binaries are host-executed from the plugin root and are not mounted into docker-mode groups (#393)")
			return
		}
		mapped, err := pluginmcp.MapStdio(srv, result.Plugin.Root)
		if err != nil {
			slog.Warn("plugin mcp server skipped", "plugin", pluginName, "server", srv.Name, "reason", err)
			return
		}
		mapped.PluginRoot = result.Plugin.Root
		dataDir, derr := plugin.PluginDataDir(home, pluginName)
		if derr != nil {
			// A plugin-sourced server without a derived PLUGIN_DATA would
			// launch with an empty data path (spec §9.1 contract broken):
			// skip it with a report instead (#402 review).
			slog.Warn("plugin mcp server skipped", "plugin", pluginName, "server", srv.Name, "reason", derr)
			return
		}
		mapped.PluginData = dataDir
		client, err := mcp.ConnectRestricted(ctx, mapped, mcp.RestrictOpts{Mode: "restricted"})
		if err != nil {
			slog.Warn("plugin mcp server failed", "plugin", pluginName, "server", srv.Name, "reason", err)
			return
		}
		*closers = append(*closers, Cleanup(func(cleanupCtx context.Context) error {
			if _, hasDeadline := cleanupCtx.Deadline(); !hasDeadline {
				return client.Close()
			}
			return client.CloseContext(cleanupCtx)
		}))
		tb, err := client.Toolbox(ctx)
		if err != nil {
			_ = client.Close()
			slog.Warn("plugin mcp server failed", "plugin", pluginName, "server", srv.Name, "reason", err)
			return
		}
		*boxes = append(*boxes, tb)
	default:
		slog.Warn("plugin mcp server skipped", "plugin", pluginName, "server", srv.Name, "reason", "unknown transport")
	}
}
