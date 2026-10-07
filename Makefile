lint:
	golangci-lint run -c ./golangci.yml ./...

test:
	go test ./... -v --cover

jstypes:
	go run ./plugins/jsvm/internal/types/types.go

test-report:
	go test ./... -v --cover -coverprofile=coverage.out
	go tool cover -html=coverage.out

# Build profiles (tag sets live in profiles.txt, see docs/PROFILES.md).
# Cross-compile with e.g. `make edge GOOS=linux GOARCH=arm64`.
PROFILE_LDFLAGS ?= -s -w
profile_tags = $(shell awk -v p="$(1)" '$$1==p {for (i=3;i<=NF;i++) printf "%s ", $$i}' profiles.txt)

.PHONY: solo team cluster edge nano profiles
solo team cluster edge nano:
	CGO_ENABLED=0 go build -trimpath -tags "$(call profile_tags,$@)" -ldflags "$(PROFILE_LDFLAGS)" -o out/toki-$@ ./examples/base
	@ls -l out/toki-$@ | awk '{printf "toki-$@ %.1f MiB\n", $$5/1048576}'

profiles: solo edge nano cluster

# Mobile bindings (gomobile; see docs/EMBED.md). Not built in CI.
.PHONY: aar xcframework
aar:
	./mobile/build.sh android
xcframework:
	./mobile/build.sh ios
