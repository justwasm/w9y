FROM btwiuse/arch:golang AS build

WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN go build -o /out/w9y ./cmd/w9y

FROM btwiuse/arch:golang

WORKDIR /app
COPY --from=build /out/w9y /usr/local/bin/w9y
CMD ["w9y"]
