package agentbuild

import (
	"context"
	"log/slog"
	"slices"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/mcp"
	"github.com/matt-riley/waffle/internal/plugin"
	"github.com/matt-riley/waffle/internal/pluginmcp"
	"github.com/matt-riley/waffle/internal/tool"
)

// wirePluginMCPServer connects one validated portable server entry with the
// most restrictive default posture (#391/#394): every failure — mapping,
// connect, or toolbox — is skipped with a structured log line naming the
// plugin, server, and reason (spec §7.2.2 rule 5 / §11.3), and never fails
// the agent build. The waffle extension policy for this server (execution,
// egress, groups, token) may grant more than the portable default, but the
// #77/#79/#249 posture still bounds its application: docker-mode groups
// refuse direct egress and host-executed plugin stdio binaries.
func (b *Builder) wirePluginMCPServer(ctx context.Context, boxes *[]tool.Toolbox, closers *[]Cleanup, redactors *[]func(string) string, result plugin.LoadResult, srv plugin.MCPServer, policy plugin.WaffleMCSPolicy, home, sandboxMode, group string) {
	pluginName := result.Plugin.Manifest.Name
	switch srv.Type {
	case plugin.MCPTypeSSE:
		return // unsupported transport, already skipped at load
	case plugin.MCPTypeStreamableHTTP:
		b.wirePluginRemoteMCP(ctx, boxes, closers, redactors, result, srv, policy, sandboxMode, group)
		return
	case plugin.MCPTypeStdio:
		if sandboxMode == "docker" {
			slog.Warn("plugin stdio mcp server refused",
				"plugin", pluginName, "server", srv.Name,
				"reason", "plugin binaries are host-executed from the plugin root and are not mounted into docker-mode groups (#393)")
			return
		}
		if len(policy.Groups) > 0 && !slices.Contains(policy.Groups, group) {
			return // group gating from the waffle extension
		}
		execution := policy.Execution
		if execution == "" {
			execution = "host"
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
		// Sandbox execution on a host-mode group launches via the #77
		// restricted executor with the workspace as working directory;
		// host execution uses the plugin root cwd from the mapping.
		launch, ropts := mcp.PlanLaunch(mapped, execution, sandboxMode, b.Config.Sandbox.WorkDir, b.Config.Sandbox.Image, b.Config.Sandbox.Network)
		client, err := mcp.ConnectRestricted(ctx, launch, ropts)
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

// wirePluginRemoteMCP connects a plugin remote server when the waffle
// extension supplies policy (egress + optional token); without it, the
// server is refused (the portable surface carries no credentials). The
// connection reuses connectRemoteMCP, so the full #249 posture applies:
// docker-mode groups must traverse the broker or are refused, unattended
// tiers stay deny-by-default unless the extension names the group, and
// credentials come only from the secret store.
func (b *Builder) wirePluginRemoteMCP(ctx context.Context, boxes *[]tool.Toolbox, closers *[]Cleanup, redactors *[]func(string) string, result plugin.LoadResult, srv plugin.MCPServer, policy plugin.WaffleMCSPolicy, sandboxMode, group string) {
	pluginName := result.Plugin.Manifest.Name
	if policy.Egress == "" {
		slog.Warn("plugin remote mcp server refused",
			"plugin", pluginName, "server", srv.Name,
			"reason", "portable mcp.json carries no egress policy; grant egress (and a token) via the waffle extension namespace (#394)")
		return
	}
	_, opts, err := pluginmcp.MapHTTP(srv)
	if err != nil {
		slog.Warn("plugin remote mcp server skipped", "plugin", pluginName, "server", srv.Name, "reason", err)
		return
	}
	synth := config.MCPServer{
		Name:    srv.Name,
		URL:     srv.URL,
		Egress:  policy.Egress,
		Groups:  policy.Groups,
		Token:   policy.Token,
		Headers: srv.Headers,
	}
	if !RemoteServerInGroup(synth, group) {
		// Unattended tiers are deny-by-default for remote servers (#249).
		return
	}
	tb, closer, redact, err := b.connectRemoteMCP(ctx, synth, group, sandboxMode, opts.Headers)
	if err != nil {
		slog.Warn("plugin remote mcp server failed", "plugin", pluginName, "server", srv.Name, "reason", err)
		return
	}
	*closers = append(*closers, closer)
	if redact != nil {
		*redactors = append(*redactors, redact)
	}
	*boxes = append(*boxes, tb)
}
