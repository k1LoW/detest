default: test

# The race detector runs in its own CI job: coverage under -race is atomic,
# and the parallel explorations contending on its counters run several times
# slower than either alone.
ci:
	go test ./... -timeout 30m -coverprofile=coverage.out -covermode=count

test:
	go test ./... -coverprofile=coverage.out -covermode=count

race:
	go test ./... -race -timeout 30m

lint:
	golangci-lint run ./...

depsdev:
	go install github.com/Songmu/ghch/cmd/ghch@latest
	go install github.com/securego/gosec/v2/cmd/gosec@latest

credits:
	go install github.com/Songmu/gocredits/cmd/gocredits@v1.0.0
	gocredits . > CREDITS
	cat _EXTRA_CREDITS >> CREDITS

prerelease:
	git pull origin main --tag
	go mod tidy
	ghch -w -N ${VER}
	$(MAKE) credits
	git add CHANGELOG.md CREDITS go.mod go.sum
	git commit -m'Bump up version number'
	git tag ${VER}

prerelease_for_tagpr:
	$(MAKE) credits
	git add CHANGELOG.md CREDITS go.mod go.sum

.PHONY: default test credits
