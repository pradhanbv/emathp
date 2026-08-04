# Multi-stage build producing all three binaries from one build context -
# one Go toolchain, three targets (gateway, mocksf, mockzd) selected via
# --target at the compose service level. Not TDD'd (IMPLEMENTATION_PLAN.md
# ground rule 4).
# -bookworm pins the builder's glibc to Debian 12, matching the distroless
# -debian12 runtime images below. The default golang tag tracks a newer
# Debian, and the cgo binary then fails at start with
# "GLIBC_2.38 not found" - a mismatch the CGO_ENABLED=0 targets never see,
# because a static binary carries no glibc dependency at all.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gateway ./cmd/gateway
# The DuckDB build is separate on purpose. `-tags duckdb` pulls in
# go-duckdb, which is cgo, so this one cannot be CGO_ENABLED=0 and its
# runtime image cannot be distroless/static - it needs libc. Keeping it a
# distinct target is what lets the default gateway stay a static binary in a
# static base image (ADR-007: the cgo cost is opt-in, not a tax on every
# deploy).
RUN CGO_ENABLED=1 go build -tags duckdb -o /out/gateway-duckdb ./cmd/gateway
RUN CGO_ENABLED=0 go build -o /out/mocksf ./cmd/mocksf
RUN CGO_ENABLED=0 go build -o /out/mockzd ./cmd/mockzd

FROM gcr.io/distroless/static-debian12 AS gateway
COPY --from=build /out/gateway /gateway
COPY testdata /testdata
WORKDIR /
ENTRYPOINT ["/gateway"]

# cc-debian12, not static-debian12 or even base-debian12. DuckDB is a C++
# library, so the cgo binary links against libstdc++ as well as glibc, and
# base-debian12 carries only the latter - it starts and then dies with
# "libstdc++.so.6: cannot open shared object file". distroless/cc is the
# variant that ships the C++ runtime.
FROM gcr.io/distroless/cc-debian12 AS gateway-duckdb
COPY --from=build /out/gateway-duckdb /gateway
COPY testdata /testdata
WORKDIR /
ENTRYPOINT ["/gateway"]

FROM gcr.io/distroless/static-debian12 AS mocksf
COPY --from=build /out/mocksf /mocksf
ENTRYPOINT ["/mocksf"]

FROM gcr.io/distroless/static-debian12 AS mockzd
COPY --from=build /out/mockzd /mockzd
ENTRYPOINT ["/mockzd"]
