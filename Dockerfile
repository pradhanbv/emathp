# Multi-stage build producing all three binaries from one build context -
# one Go toolchain, three targets (gateway, mocksf, mockzd) selected via
# --target at the compose service level. Not TDD'd (IMPLEMENTATION_PLAN.md
# ground rule 4).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gateway ./cmd/gateway
RUN CGO_ENABLED=0 go build -o /out/mocksf ./cmd/mocksf
RUN CGO_ENABLED=0 go build -o /out/mockzd ./cmd/mockzd

FROM gcr.io/distroless/static-debian12 AS gateway
COPY --from=build /out/gateway /gateway
COPY testdata /testdata
WORKDIR /
ENTRYPOINT ["/gateway"]

FROM gcr.io/distroless/static-debian12 AS mocksf
COPY --from=build /out/mocksf /mocksf
ENTRYPOINT ["/mocksf"]

FROM gcr.io/distroless/static-debian12 AS mockzd
COPY --from=build /out/mockzd /mockzd
ENTRYPOINT ["/mockzd"]
