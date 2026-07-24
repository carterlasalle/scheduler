# Support

## Documentation

- [README.md](README.md) — Project overview, features, configuration
- [docs/fleet.md](docs/fleet.md) — Fleet management guide
- [specs/](specs/) — Architecture and design specifications (S01-S11)

## Getting Help

For bugs, feature requests, or questions:

1. Check existing [GitHub Issues](https://github.com/coding-hermes/scheduler/issues)
2. Open a new issue with:
   - Scheduler version (`schedulerd --version` or `git describe --tags`)
   - Go version (`go version`)
   - Relevant logs or error messages
   - Steps to reproduce

## Configuration Issues

Most issues stem from configuration. Verify:

- `--gateway-url` points to a running Hermes Gateway
- `--gateway-key` is valid
- `--db` path is writable
- `--max-concurrent` does not exceed your system's process limits
