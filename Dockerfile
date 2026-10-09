FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -o /out/mockpsp ./cmd/mockpsp

FROM gcr.io/distroless/static-debian12:nonroot AS api
COPY --from=build /out/api /api
ENTRYPOINT ["/api"]

FROM gcr.io/distroless/static-debian12:nonroot AS mockpsp
COPY --from=build /out/mockpsp /mockpsp
ENTRYPOINT ["/mockpsp"]
