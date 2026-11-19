FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
# Кэш переводов; в проде сюда монтируется том.
COPY --from=build --chown=nonroot:nonroot /tmp /data
ENV CACHE_PATH=/data/translations.json IDEAS_PATH=/data/ideas.json LAST_PATH=/data/last.json
EXPOSE 8080
ENTRYPOINT ["/server"]
