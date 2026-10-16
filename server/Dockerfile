FROM golang:1.23 AS build
WORKDIR /src
COPY . .
ARG CMD=api
RUN CGO_ENABLED=0 go build -o /app ./cmd/$CMD

FROM gcr.io/distroless/static
COPY --from=build /app /app
ENTRYPOINT ["/app"]
