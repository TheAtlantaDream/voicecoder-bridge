FROM golang:1.25-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bridge ./cmd/bridge

FROM alpine:3.20
COPY --from=build /bridge /bridge
EXPOSE 8000
ENTRYPOINT ["/bridge"]
