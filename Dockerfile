FROM golang:1.25 AS build
WORKDIR /src
COPY go.* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/godis-shortener .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/godis-shortener /godis-shortener
EXPOSE 8090
ENTRYPOINT ["/godis-shortener"]
