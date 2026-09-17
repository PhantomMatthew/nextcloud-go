package plugins

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Plugin is a compiled guest module.
type Plugin struct {
	host     *Host
	manifest *Manifest
	compiled wazero.CompiledModule
	inst     api.Module
}

// Install instantiates the module, checks ABI 1, and runs on_install if set.
func (p *Plugin) Install(ctx context.Context) error {
	if p == nil || p.host == nil {
		return fmt.Errorf("plugins: nil plugin")
	}
	if p.inst != nil {
		_ = p.inst.Close(ctx)
		p.inst = nil
	}
	name := p.manifest.Plugin.ID
	cfg := wazero.NewModuleConfig().WithName(name).WithStartFunctions()
	timeout := p.host.cfg.DefaultCallTimeout
	if p.manifest.Runtime.CPUTimeoutMS > 0 {
		timeout = time.Duration(p.manifest.Runtime.CPUTimeoutMS) * time.Millisecond
	}
	callCtx, cancel := context.WithTimeout(withPlugin(ctx, p.manifest.Plugin.ID, p.manifest.Plugin.Version), timeout)
	defer cancel()

	inst, err := p.host.rt.InstantiateModule(callCtx, p.compiled, cfg)
	if err != nil {
		return wrapTrap(err)
	}
	p.inst = inst

	fn := inst.ExportedFunction("ncgo_abi_version")
	if fn == nil {
		return ErrMissingExport
	}
	results, err := fn.Call(callCtx)
	if err != nil {
		return wrapTrap(err)
	}
	if len(results) == 0 || int32(results[0]) != 1 { //nolint:gosec // G115: ABI version is 0 or 1
		return ErrABIMismatch
	}

	on := p.manifest.EntryPoints.OnInstall
	if on == "" {
		return nil
	}
	entry := inst.ExportedFunction(on)
	if entry == nil {
		return fmt.Errorf("%w: %s", ErrMissingExport, on)
	}
	results, err = entry.Call(callCtx)
	if err != nil {
		return wrapTrap(err)
	}
	if len(results) > 0 {
		code := int32(results[0]) //nolint:gosec // G115: plugin i32 return
		if code != 0 {
			return &PluginError{Code: code}
		}
	}
	return nil
}

// Close releases the instance.
func (p *Plugin) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var err error
	if p.inst != nil {
		err = p.inst.Close(ctx)
		p.inst = nil
	}
	if p.compiled != nil {
		err = errors.Join(err, p.compiled.Close(ctx))
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
