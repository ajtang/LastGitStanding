# One image, every service; the Deployment's args pick which one runs. See ADR-0007.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY gen gen
COPY internal internal
COPY services services
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lgs ./cmd/lgs

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/lgs /lgs
ENTRYPOINT ["/lgs"]
