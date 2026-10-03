# Single static binaries. Everything outside internal/issue and internal/review
# is entity-agnostic, so a third type is a package and a line here.
BINS    := git-issue git-review
PREFIX  ?= /usr/local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# CGO_ENABLED=0 is the whole point: a static binary is why this is Go at all
# (see AGENTS.md, "Decisions already made"). -trimpath keeps builds reproducible.
GO      := CGO_ENABLED=0 go
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)

TARGETS := $(addprefix bin/,$(BINS))

.PHONY: all build test vet fmt install uninstall clean
all: build
build: $(TARGETS)

$(TARGETS): bin/%: $(shell find . -name '*.go' -not -path './bin/*') go.mod
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/$*

# Goldens are captured from a fixed timezone; the CLI tests re-pin it per-process.
test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

# `git issue --help` is git's own rewrite of `git help issue`, which runs
# `man git-issue` — so the page has to be on the man path, not just in the repo.
MANPAGES := $(addsuffix .1,$(addprefix man/,$(BINS)))

install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(TARGETS) $(DESTDIR)$(PREFIX)/bin/
	install -d $(DESTDIR)$(PREFIX)/share/man/man1
	install -m 0644 $(MANPAGES) $(DESTDIR)$(PREFIX)/share/man/man1/

uninstall:
	rm -f $(addprefix $(DESTDIR)$(PREFIX)/bin/,$(BINS))
	rm -f $(addprefix $(DESTDIR)$(PREFIX)/share/man/man1/,$(notdir $(MANPAGES)))

clean:
	rm -rf bin
