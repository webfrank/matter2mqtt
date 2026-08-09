FROM golang:1.26-alpine AS build
WORKDIR /src

# go.sum is optional here: the glob matches nothing on a fresh clone.
COPY go.mod go.sum* ./

# Populates the module cache. Cached as its own layer when go.sum is committed,
# so dependency downloads are skipped on source-only rebuilds.
RUN go mod download

COPY *.go model.json ./
COPY web ./web

# Since Go 1.18 `go mod download` no longer records module content hashes, so a
# fresh clone without go.sum reaches the build with an incomplete file and fails
# with "missing go.sum entry". tidy writes the missing hashes; it is a no-op
# once go.sum is committed. Commit it - that is what pins the dependency tree.
RUN go mod tidy

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/matter2mqtt .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/matter2mqtt /matter2mqtt
USER nonroot:nonroot
EXPOSE 8099
ENTRYPOINT ["/matter2mqtt"]
