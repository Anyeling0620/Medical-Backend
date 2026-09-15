FROM golang:1.26 AS build

ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -o /out/medical-server ./cmd/server

FROM scratch

# 复制根证书，保证调用支付宝、微信等 HTTPS 服务时能够完成 TLS 校验。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/medical-server /medical-server

EXPOSE 9080
ENTRYPOINT ["/medical-server"]
