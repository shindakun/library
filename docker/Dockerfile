# Go library service: single static binary on a distroless base.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/library ./cmd/library

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/library /library
EXPOSE 8080
ENTRYPOINT ["/library"]
