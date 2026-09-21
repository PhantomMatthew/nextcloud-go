package plugins

import (
	"context"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
)

// Plugin is a compiled guest module with its instance manager.
type Plugin struct {
	host     *Host
	manifest *Manifest
	compiled wazero.CompiledModule
	manager  *instanceManager
}

func (p *Plugin) callTimeout() time.Duration {
	if p.manifest.Runtime.CPUTimeoutMS > 0 {
		return time.Duration(p.manifest.Runtime.CPUTimeoutMS) * time.Millisecond
	}
	return p.host.cfg.DefaultCallTimeout
}

// Call invokes an exported entry point with the manifest's per-call timeout.
// Per-call metadata (user, request id, locale, deadline) is taken from a
// CallContext attached with WithCallContext. A trap destroys the instance.
// Non-zero i32 results are returned as-is; interpreting them as errors is up
// to the caller (ncgo_abi_version legitimately returns 1).
func (p *Plugin) Call(ctx context.Context, entry string, args ...uint64) ([]uint64, error) {
	if p == nil || p.host == nil {
		return nil, fmt.Errorf("plugins: nil plugin")
	}
	callCtx, cancel := context.WithTimeout(withCall(ctx, p), p.callTimeout())
	defer cancel()

	inst, release, err := p.manager.acquire(callCtx)
	if err != nil {
		return nil, err
	}
	fn := inst.mod.ExportedFunction(entry)
	if fn == nil {
		release(false)
		return nil, fmt.Errorf("%w: %s", ErrMissingExport, entry)
	}
	results, err := fn.Call(callCtx, args...)
	if err != nil {
		release(true)
		return nil, wrapTrap(err)
	}
	release(false)
	return results, nil
}

// callEntry invokes an entry point and converts a non-zero i32 result into
// a PluginError.
func (p *Plugin) callEntry(ctx context.Context, entry string, args ...uint64) error {
	results, err := p.Call(ctx, entry, args...)
	if err != nil {
		return err
	}
	if len(results) > 0 {
		if code := int32(results[0]); code != 0 { //nolint:gosec // G115: plugin i32 return
			return &PluginError{Code: code}
		}
	}
	return nil
}

// Install warms instances, checks ABI 1, and runs on_install if set.
func (p *Plugin) Install(ctx context.Context) error {
	if p == nil || p.host == nil {
		return fmt.Errorf("plugins: nil plugin")
	}
	callCtx, cancel := context.WithTimeout(withPlugin(ctx, p.manifest.Plugin.ID, p.manifest.Plugin.Version), p.callTimeout())
	defer cancel()
	if err := p.manager.warm(callCtx); err != nil {
		return err
	}

	results, err := p.Call(ctx, "ncgo_abi_version")
	if err != nil {
		return err
	}
	if len(results) == 0 || int32(results[0]) != 1 { //nolint:gosec // G115: ABI version is 0 or 1
		return ErrABIMismatch
	}

	on := p.manifest.EntryPoints.OnInstall
	if on == "" {
		return nil
	}
	return p.callEntry(ctx, on)
}

// Uninstall runs on_uninstall if set and releases all instances.
func (p *Plugin) Uninstall(ctx context.Context) error {
	if p == nil || p.host == nil {
		return fmt.Errorf("plugins: nil plugin")
	}
	on := p.manifest.EntryPoints.OnUninstall
	if on != "" {
		if err := p.callEntry(ctx, on); err != nil {
			return err
		}
	}
	p.manager.closeAll()
	return nil
}

// Close releases all instances and the compiled module.
func (p *Plugin) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if p.manager != nil {
		p.manager.closeAll()
	}
	var err error
	if p.compiled != nil {
		err = p.compiled.Close(ctx)
		p.compiled = nil
	}
	return err
}

func wrapTrap(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrTrap, err)
}
