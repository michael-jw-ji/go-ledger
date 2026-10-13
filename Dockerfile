# One image with the API and the built web console, served together:
# the console at /, the API under /api (WEB_DIR turns that on).

FROM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npx vite build

FROM golang:1.27-alpine AS api
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ledger ./cmd/ledger

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=api /out/ledger /app/ledger
COPY --from=web /web/dist /app/web
ENV WEB_DIR=/app/web
EXPOSE 8088
ENTRYPOINT ["/app/ledger"]
