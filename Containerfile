FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git

RUN go install github.com/a-h/templ/cmd/templ@latest

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN templ generate
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /cozytempl ./cmd/cozytempl

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /cozytempl /cozytempl

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/cozytempl"]
