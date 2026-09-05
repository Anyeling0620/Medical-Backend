# Application image definition will be finalized with the production build pipeline.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/medical-server ./cmd/server

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/medical-server /medical-server
EXPOSE 8080
ENTRYPOINT ["/medical-server"]
