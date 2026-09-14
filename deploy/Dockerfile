FROM golang:1.26 AS build

ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN wget "http://arms-apm-cn-hangzhou.oss-cn-hangzhou.aliyuncs.com/instgo/instgo-linux-amd64" -O instgo && \
    chmod +x instgo

RUN CGO_ENABLED=0 ./instgo go build -o /out/medical-server ./cmd/server

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/medical-server /medical-server
EXPOSE 9080
ENTRYPOINT ["/medical-server"]