regproxy
========

An http reverse proxy with a dynamic list of upstreams.

Requests are forwarded to all upstreams in parallel and all are expected to be successful. Only one response is 
returned to the client.

## Use cases:

It can be used to implement a control plane for a dynamic set of services, where commands are synchronous and 
errors must be highlighted to the caller rather than ignored.

## Prerequisites
* go > 1.17 
* OR docker

## Instructions
To build:
```bash
go build -o regproxy
```

To run:
```bash
./regproxy
```

In docker
```bash
docker build . -t regproxy
# Set an alias to run as if it was a normal executable
alias regproxy="docker run --rm -it --network=host regproxy"
```

For command line options see: 
```bash
regproxy -help
```

To test: 
* start the proxy
* start up 2 http servers on different ports
* register both those servers with the reverse proxy
* make a request to the proxy
```bash
regproxy&
python -m http.server 3000&
python -m http.server 3001&
curl -X PUT http://localhost:9876/register -d '{"name": "upstream-1", "callback": "http://localhost:3000"}'
curl -X PUT http://localhost:9876/register -d '{"name": "upstream-2", "callback": "http://localhost:3001"}'
curl http://localhost:9877/
```
Then you can stop one upstream, and see requests will now fail
```bash
kill %1                     # kill the first python server
curl http://localhost:9877/ # Should fail
```

## Extensions:

* Replace the in-memory list with a service discovery system e.g. netflix eureka
