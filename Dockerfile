FROM golang:1.25-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bridge ./cmd/bridge

FROM alpine:3.20
# git reads the project diff. The user id matches the agent and the Coder
# workspace, which own the project folder.
RUN apk add --no-cache git
COPY --from=build /bridge /bridge
USER 1000:1000
EXPOSE 8000
ENTRYPOINT ["/bridge"]
