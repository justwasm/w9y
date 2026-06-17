FROM btwiuse/arch:golang AS build

COPY . /app
WORKDIR /app
RUN go mod tidy
RUN go build -o /bin/w9y ./cmd/w9y

FROM btwiuse/arch:tinygo

WORKDIR /app
COPY --from=build /bin/w9y /bin/w9y
COPY ./start.sh /app/start.sh

CMD ["/app/start.sh"]
