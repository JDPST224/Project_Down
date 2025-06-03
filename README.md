## Start Agent
```bash
ulimit -n 999999
go build l7.go
go run Agent.go
```
## Start Control Server
```bash
go run Control_Server.go
```
