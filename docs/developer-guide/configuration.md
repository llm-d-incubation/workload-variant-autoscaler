## Unified Configuration System

WVA uses a unified configuration system that consolidates all settings into a single `Config` structure. This provides clear precedence rules, type safety, and separation between static (immutable) and dynamic (runtime-updatable) configuration.

### Configuration Structure

The unified `Config` consists of two parts:

1. **StaticConfig**: Immutable settings loaded at startup (require controller restart to change)
   - Infrastructure settings (metrics/probe addresses, leader election)
   - Connection settings (Prometheus URL, TLS certificates)
   - Feature flags
   - GPU limiter selection (`--limiter-type`, `--quota-config-file`)

2. **DynamicConfig**: Runtime-updatable settings (can be changed via ConfigMap updates)
   - Optimization interval
   - Saturation scaling thresholds
   - Scale-to-zero configuration
   - Prometheus cache settings

### GPU limiter

The GPU resource limiter is selected at startup via `--limiter-type` (or
`LIMITER_TYPE`):

- `inventory` (default) — caps scaling at the physically discovered GPU
  inventory.
- `quota` — enforces operator-declared per-accelerator-type caps at cluster or
  namespace scope, loaded from `--quota-config-file` (or `QUOTA_CONFIG_FILE`).

Both are **StaticConfig**: the quota file is read once at startup and requires a
controller restart to apply changes. See the
[Quota Limiter guide](./quota-limiter.md) for the configuration schema, scopes,
validation rules, and pipeline behavior.
