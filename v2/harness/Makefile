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

.PHONY: help build test fmt fmt-check clippy lint check smoke version clean

help:
	@echo "targets: build test fmt fmt-check clippy lint check smoke version clean"
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

# Deletes target/ (GiBs). Do this before removing a worktree.
clean:
	$(CARGO) clean
