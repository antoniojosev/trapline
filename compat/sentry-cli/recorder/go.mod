// The recorder is its own module so that a development-only HTTP server, and
// anything it ever needs, stays out of the product's dependency graph — the
// same reason each SDK suite in compat/ is its own module.
module trapline.local/compat/sentry-cli/recorder

go 1.26.1

toolchain go1.26.6
