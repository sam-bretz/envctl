package agent

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/guestjob"
	vm "github.com/sam-bretz/envctl/internal/runtime"
)

func exerciseNoImplicitForwarding(t *testing.T, ctx context.Context, guest guestjob.Client) {
	t.Helper()
	dir := "/work/envctl/network-fixture"
	if err := guest.Provider.Exec(ctx, guest.Runtime, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "mkdir", "-p", dir}}); err != nil {
		t.Fatal(err)
	}
	for _, bind := range []string{"127.0.0.1", "0.0.0.0"} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()
		job := fmt.Sprintf("network_%d", time.Now().UnixNano())
		program := `import socket,sys,threading
host,port=sys.argv[1],int(sys.argv[2])
def udp():
 s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind((host,port))
 while True:
  data,address=s.recvfrom(1024);s.sendto(b'envctl-network-fixture',address)
threading.Thread(target=udp,daemon=True).start()
s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind((host,port));s.listen()
while True:
 c,_=s.accept();c.sendall(b'envctl-network-fixture');c.close()
`
		if _, err = guest.Submit(ctx, guestjob.Request{ID: job, Args: []string{"python3", "-c", program, bind, strconv.Itoa(port)}, Dir: dir, TimeoutSeconds: 30}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := guest.Cancel(cleanup, job); err != nil {
				t.Log(err)
			}
		})
		probe := `import socket,sys,time
p=int(sys.argv[1])
for attempt in range(30):
 try:
  s=socket.create_connection(('127.0.0.1',p),1);assert s.recv(100)==b'envctl-network-fixture';s.close()
  s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(1);s.sendto(b'ping',('127.0.0.1',p));assert s.recv(100)==b'envctl-network-fixture';s.close();break
 except OSError:
  time.sleep(.1)
else:
 raise RuntimeError('fixture did not start')
`
		if err = guest.Provider.Exec(ctx, guest.Runtime, vm.Command{Args: []string{"python3", "-c", probe, strconv.Itoa(port)}}); err != nil {
			t.Fatal("guest listeners were not reachable inside the VM", err)
		}
		// Give Lima's asynchronous listener detection time to react, then prove
		// that neither protocol was silently exposed on the host loopback.
		for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); {
			address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			if conn, err := net.DialTimeout("tcp", address, 150*time.Millisecond); err == nil {
				conn.Close()
				t.Fatalf("guest %s TCP listener was automatically forwarded", bind)
			}
			conn, err := net.DialTimeout("udp", address, 150*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			conn.SetDeadline(time.Now().Add(150 * time.Millisecond))
			conn.Write([]byte("ping"))
			buffer := make([]byte, 100)
			if n, err := conn.Read(buffer); err == nil && n > 0 {
				conn.Close()
				t.Fatalf("guest %s UDP listener was automatically forwarded", bind)
			}
			conn.Close()
			time.Sleep(100 * time.Millisecond)
		}
		if _, err = guest.Cancel(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("guest loopback/wildcard TCP and UDP listeners are reachable in the VM but not automatically forwarded to the host")
}
