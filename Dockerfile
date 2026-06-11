FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/market-server ./cmd/market-server

FROM alpine:3.22

WORKDIR /app

COPY --from=build /out/market-server /usr/local/bin/market-server

ENV MARKET_ADDR=:8088
ENV MARKET_DB_PATH=/data/market.db
ENV MARKET_ARTIFACT_ROOT=/data/artifacts

VOLUME ["/data"]
EXPOSE 8088

ENTRYPOINT ["market-server"]
