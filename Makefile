# One-time machine bootstrap. Everything after this is `mise run <task>`
# (see mise.toml, or `mise tasks`).

SHELL := /bin/bash
.DEFAULT_GOAL := bootstrap

# Use mise from PATH if present, otherwise the standard install location.
MISE ?= $(or $(shell command -v mise 2>/dev/null),$(HOME)/.local/bin/mise)
# Pin the mise version for reproducible installs (empty = latest).
MISE_VERSION ?=

.PHONY: bootstrap

bootstrap: $(MISE)
	@shell_name=$$(basename "$${SHELL:-}"); \
	case "$$shell_name" in \
	  zsh)  rc="$$HOME/.zshrc";  line='eval "$$($(MISE) activate zsh)"' ;; \
	  bash) rc="$$HOME/.bashrc"; [ "$$(uname)" = Darwin ] && rc="$$HOME/.bash_profile"; \
	        line='eval "$$($(MISE) activate bash)"' ;; \
	  fish) rc="$$HOME/.config/fish/config.fish"; line='$(MISE) activate fish | source' ;; \
	  *) echo "Unsupported shell '$$shell_name': activate mise manually, see https://mise.jdx.dev/getting-started.html" >&2; exit 1 ;; \
	esac; \
	if grep -qs 'mise activate' "$$rc"; then \
	  echo "==> mise already activated in $$rc"; \
	else \
	  mkdir -p "$$(dirname "$$rc")"; \
	  printf '\n# mise: per-directory tool versions (https://mise.jdx.dev)\n%s\n' "$$line" >> "$$rc"; \
	  echo "==> Added mise activation to $$rc"; \
	fi
	@$(MISE) trust --yes >/dev/null 2>&1
	@$(MISE) install
	@echo
	@echo "Done. Restart your shell (exec \$$SHELL), then: mise tasks"

$(MISE):
	@command -v curl >/dev/null || { echo "error: curl is required to install mise" >&2; exit 1; }
	@echo "==> Installing mise to $@"
	curl -fsSL https://mise.run | MISE_INSTALL_PATH="$@" $(if $(MISE_VERSION),MISE_VERSION="$(MISE_VERSION)") sh
