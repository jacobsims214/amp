# AMP harness dev loop (D13). Run from anywhere: make -C v2/harness <target>
#
# Rust disk rule: cargo's registry/git caches must not land on the dev Mac's nearly full internal disk.
# When the external cache dir exists, every target here uses it, because agent shells don't load ~/.zshrc.
CARGO_HOME_EXTERNAL := /Volumes/external-data/development/.cargo-home
ifneq ($(wildcard $(CARGO_HOME_EXTERNAL)),)
export CARGO_HOME := $(CARGO_HOME_EXTERNAL)
endif

CARGO ?= cargo
BIN := target/debug/amp-harness
# Where `make install` puts the binary (a bin directory, no sudo needed).
PREFIX ?= $(HOME)/.local/bin

.PHONY: help build test fmt fmt-check clippy lint check smoke version clean assets assets-check \
	release dist install install-test install-test-distros

help:
	@echo "targets: build test fmt fmt-check clippy lint check smoke version clean assets assets-check"
	@echo "         release dist install [PREFIX=$(PREFIX)] install-test install-test-distros"
	@echo "CARGO_HOME=$${CARGO_HOME:-<unset: ~/.cargo>}"

build:
	$(CARGO) build --workspace --all-targets --locked

test:
	$(CARGO) test --workspace --locked

fmt:
	$(CARGO) fmt --all

fmt-check:
	$(CARGO) fmt --all -- --check

clippy:
	$(CARGO) clippy --workspace --all-targets --locked -- -D warnings

# What CI's `harness` job runs.
lint: fmt-check clippy
check: lint test

smoke: build
	./scripts/smoke.sh

version: build
	@$(BIN) --version

# Packaging (#2115, D12). `release` builds the optimized host binary; `dist` packs it as
# target/dist/amp-harness-<version>-<host>.tar.gz + .sha256 (the release workflow does the same per
# target); `install` installs that archive with install.sh, the same path users take, into PREFIX.
release:
	$(CARGO) build --release --locked -p amp-harness

dist: release
	./scripts/package.sh

# --no-setup (#2330): make runs install.sh in v2/harness, so setup would link the harness folder
# instead of your project. install.sh prints the setup command to run in your project folder instead.
install: release
	archive=$$(./scripts/package.sh | tail -n 1) && ./install.sh --archive "$$archive" --prefix "$(PREFIX)" --no-setup

# Offline tests for install.sh / package.sh / render-formula.sh (stub binary, HOME under target/).
install-test:
	./scripts/test-install.sh

# install.sh inside Debian, Ubuntu (old glibc, wget only) and Alpine containers (needs docker).
install-test-distros:
	./scripts/test-install-distros.sh

# Agent/skill assets (D5): regenerate assets/ from v2/.opencode, or fail if the committed copy drifted.
assets:
	../scripts/build-harness-assets.sh

assets-check:
	../scripts/build-harness-assets.sh --check

# Deletes target/ (GiBs). Do this before removing a worktree.
clean:
	$(CARGO) clean
