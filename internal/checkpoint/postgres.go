// Package checkpoint retains restorable application data separately from VM
// lifetime and Git history. Adapters execute only in the selected guest stack.
package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/gueststack"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

const MaxDatasetBytes = 64 << 20

var id = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,100}$`)

type Client struct {
	Guest    guestjob.Client
	Stack    gueststack.Client
	Prepared gueststack.Prepared
	Store    *runstore.Store
	Revision string
}

// Postgres is retained as a source-compatible name for the original adapter.
type Postgres = Client

type operation struct {
	Action      string `json:"action"`
	Key         string `json:"key,omitempty"`
	Container   string `json:"container"`
	Database    string `json:"database"`
	User        string `json:"user"`
	SQL         string `json:"sql"`
	Equals      string `json:"equals"`
	Input       string `json:"input"`
	InputDigest string `json:"input_digest"`
	Version     string `json:"version"`
	Output      string `json:"output"`
}
type outcome struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	Digest  string `json:"digest,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Detail  string `json:"detail"`
}

func (p Client) path(dataset string) (string, error) {
	if !id.MatchString(p.Revision) || !id.MatchString(dataset) {
		return "", errors.New("invalid dataset execution identity")
	}
	return "/work/envctl/datasets/" + p.Revision + "/" + dataset, nil
}

// Seed input is a retained artifact resolved from a pinned repository.
func (p Client) Seed(ctx context.Context, operationID string, spec workflow.Dataset, seed workflow.Artifact) (workflow.DatasetSnapshot, error) {
	return p.run(ctx, operationID, spec, "seed", &seed, "")
}
func (p Client) Capture(ctx context.Context, operationID string, spec workflow.Dataset) (workflow.DatasetSnapshot, error) {
	return p.run(ctx, operationID, spec, "capture", nil, "")
}
func (p Client) Probe(ctx context.Context, operationID string, spec workflow.Dataset) error {
	_, err := p.run(ctx, operationID, spec, "probe", nil, "")
	return err
}
func (p Client) Restore(ctx context.Context, operationID string, spec workflow.Dataset, snapshot workflow.DatasetSnapshot) error {
	if snapshot.Adapter != spec.Adapter || snapshot.Format != spec.SnapshotFormat() || snapshot.BindingDigest != workflow.Digest(spec) || snapshot.ToolVersion == "" || snapshot.Source.MediaType != spec.SnapshotMedia() || snapshot.Evidence.MediaType != "application/json" {
		return errors.New("dataset snapshot does not match its adapter and configuration")
	}
	if p.Store == nil {
		return errors.New("dataset artifact store is unavailable")
	}
	if raw, err := p.Store.Artifact(snapshot.Evidence.Digest); err != nil || int64(len(raw)) != snapshot.Evidence.Size {
		return errors.New("dataset verification evidence is missing or corrupt")
	}
	_, err := p.run(ctx, operationID, spec, "restore", &snapshot.Source, snapshot.ToolVersion)
	return err
}

// Every operation is a durable guest job. Stdin carries only scoped metadata;
// large seed/dump inputs are checksummed private guest files, not arguments.
func (p Client) run(ctx context.Context, operationID string, spec workflow.Dataset, action string, input *workflow.Artifact, version string) (workflow.DatasetSnapshot, error) {
	if err := spec.Validate(); err != nil {
		return workflow.DatasetSnapshot{}, err
	}
	if !id.MatchString(operationID) || p.Store == nil {
		return workflow.DatasetSnapshot{}, errors.New("dataset requires operation identity and artifact store")
	}
	root, err := p.path(spec.ID)
	if err != nil {
		return workflow.DatasetSnapshot{}, err
	}
	container, err := p.Stack.Container(ctx, p.Prepared, spec.Service)
	if err != nil {
		return workflow.DatasetSnapshot{}, err
	}
	var raw []byte
	inputDigest := ""
	if input != nil {
		raw, err = p.Store.Artifact(input.Digest)
		if err != nil || int64(len(raw)) != input.Size || len(raw) == 0 || len(raw) > MaxDatasetBytes {
			return workflow.DatasetSnapshot{}, errors.New("dataset input is missing, corrupt, empty, or exceeds 64 MiB")
		}
		inputDigest = input.Digest
	}
	identity := workflow.Digest(struct {
		Revision, Operation, Action, Container, Input, Version string
		Spec                                                   workflow.Dataset
	}{p.Revision, operationID, action, container, inputDigest, version, spec})
	jobID := "data_" + identity[:40]
	inputFile := ""
	if input != nil {
		inputFile = root + "/input-" + inputDigest
	}
	setup := `import hashlib,os,pwd,sys,tempfile
root,digest=sys.argv[1:]
os.makedirs(root,mode=0o755,exist_ok=True)
state=root+'/state';os.makedirs(state,mode=0o700,exist_ok=True)
agent=pwd.getpwnam('envctl-agent');os.chown(state,agent.pw_uid,agent.pw_gid)
if digest:
 raw=sys.stdin.buffer.read(64*1024*1024+1)
 if hashlib.sha256(raw).hexdigest()!=digest or len(raw)>64*1024*1024:raise RuntimeError('input integrity')
 dest=root+'/input-'+digest
 if os.path.exists(dest):
  if hashlib.sha256(open(dest,'rb').read()).hexdigest()!=digest:raise RuntimeError('input changed')
 else:
  fd,tmp=tempfile.mkstemp(prefix='.input-',dir=root)
  try:
   with os.fdopen(fd,'wb') as f:f.write(raw);f.flush();os.fsync(f.fileno())
   os.chmod(tmp,0o444);os.replace(tmp,dest)
  finally:
   if os.path.exists(tmp):os.unlink(tmp)
`
	if err = p.Guest.Provider.Exec(ctx, p.Guest.Runtime, vm.Command{Args: []string{"sudo", "python3", "-c", setup, root, inputDigest}, Stdin: bytes.NewReader(raw), Stderr: io.Discard}); err != nil {
		return workflow.DatasetSnapshot{}, errors.New("dataset input transfer failed")
	}
	output := root + "/state/" + jobID + ".dump"
	script := postgresOperation
	if spec.Adapter == "http-fixture" {
		script = fixtureOperation
	}
	op := operation{Key: spec.VerifyKey, Action: action, Container: container, Database: spec.Database, User: spec.User, SQL: spec.VerifySQL, Equals: spec.VerifyEquals, Input: inputFile, InputDigest: inputDigest, Version: version, Output: output}
	request, _ := json.Marshal(op)
	if _, err = p.Guest.Submit(ctx, guestjob.Request{ID: jobID, Args: []string{"python3", "-c", script}, Dir: root + "/state", Input: string(request), TimeoutSeconds: 300}); err != nil {
		return workflow.DatasetSnapshot{}, err
	}
	var logs strings.Builder
	var cursor int64
	for {
		status, e := p.Guest.Poll(ctx, jobID, cursor)
		if e != nil {
			return workflow.DatasetSnapshot{}, e
		}
		logs.WriteString(status.Output)
		cursor = status.Cursor
		if status.Truncated || logs.Len() > 1<<20 {
			return workflow.DatasetSnapshot{}, errors.New("dataset operation exceeded its evidence bound")
		}
		if status.Output != "" {
			continue
		}
		if status.State == "completed" {
			break
		}
		if status.State != "running" && status.State != "starting" && status.State != "pending" {
			return workflow.DatasetSnapshot{}, errors.New("dataset operation failed; its scoped guest journal is retained")
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return workflow.DatasetSnapshot{}, ctx.Err()
		case <-timer.C:
		}
	}
	var result outcome
	if json.Unmarshal([]byte(logs.String()), &result) != nil || !result.OK || result.Version == "" {
		return workflow.DatasetSnapshot{}, errors.New("dataset operation did not verify its result")
	}
	evidence, err := p.Store.PutArtifact("dataset."+spec.ID+".verification", "application/json", []byte(logs.String()))
	if err != nil {
		return workflow.DatasetSnapshot{}, err
	}
	snapshot := workflow.DatasetSnapshot{Adapter: spec.Adapter, BindingDigest: workflow.Digest(spec), Format: spec.SnapshotFormat(), ToolVersion: result.Version, Evidence: evidence}
	if action == "probe" || action == "restore" {
		return snapshot, nil
	}
	var dump boundedDump
	if result.Size < 1 || result.Size > MaxDatasetBytes {
		return snapshot, errors.New("dataset dump size is invalid")
	}
	if err = p.Guest.Provider.Exec(ctx, p.Guest.Runtime, vm.Command{Args: []string{"sudo", "cat", output}, Stdout: &dump, Stderr: io.Discard}); err != nil {
		return snapshot, errors.New("dataset dump transfer failed")
	}
	sum := sha256.Sum256(dump.Bytes())
	if hex.EncodeToString(sum[:]) != result.Digest || int64(dump.Len()) != result.Size {
		return snapshot, errors.New("dataset dump integrity check failed")
	}
	snapshot.Source, err = p.Store.PutArtifact("dataset."+spec.ID, spec.SnapshotMedia(), dump.Bytes())
	return snapshot, err
}

type boundedDump struct{ bytes.Buffer }

func (b *boundedDump) Write(raw []byte) (int, error) {
	if b.Len()+len(raw) > MaxDatasetBytes {
		return 0, fmt.Errorf("dataset exceeds %d bytes", MaxDatasetBytes)
	}
	return b.Buffer.Write(raw)
}

const postgresOperation = `import hashlib,json,os,resource,subprocess,sys
resource.setrlimit(resource.RLIMIT_FSIZE,(64*1024*1024,64*1024*1024))
r=json.load(sys.stdin)
docker=['docker','--host','unix:///var/run/docker.sock','exec','-i',r['container']]
auth=['--host=/var/run/postgresql','--username='+r['user'],'--no-password']
def run(args,body=None,out=subprocess.PIPE):
 p=subprocess.run(docker+args,input=body,stdout=out,stderr=subprocess.DEVNULL)
 if p.returncode:raise RuntimeError('database command failed')
 return p.stdout
def verify():
 value=run(['psql']+auth+['--dbname='+r['database'],'--no-psqlrc','--quiet','--tuples-only','--no-align','--set=ON_ERROR_STOP=1'],('BEGIN READ ONLY;\n'+r['sql']+'\n;COMMIT;\n').encode())
 if value.decode().strip()!=r['equals'].strip():raise RuntimeError('application verification failed')
def rebuild():
 run(['dropdb']+auth+['--maintenance-db=postgres','--if-exists','--force',r['database']])
 run(['createdb']+auth+['--maintenance-db=postgres','--template=template0',r['database']])
version=run(['pg_dump','--version']).decode().strip()
if r['version'] and r['version']!=version:raise RuntimeError('dump tool version mismatch')
if r['input']:
 data=open(r['input'],'rb').read()
 if hashlib.sha256(data).hexdigest()!=r['input_digest']:raise RuntimeError('input integrity failed')
if r['action']=='seed':
 rebuild()
 run(['psql']+auth+['--dbname='+r['database'],'--no-psqlrc','--single-transaction','--set=ON_ERROR_STOP=1'],data)
if r['action']=='restore':
 rebuild()
 run(['pg_restore']+auth+['--dbname='+r['database'],'--single-transaction','--exit-on-error','--no-owner','--no-privileges'],data)
verify()
result={'ok':True,'version':version,'detail':'application verification passed after '+r['action']}
if r['action'] in ['seed','capture']:
 tmp=r['output']+'.tmp'
 with open(tmp,'wb') as f:
  run(['pg_dump']+auth+['--dbname='+r['database'],'--format=custom','--no-owner','--no-privileges'],out=f)
  f.flush();os.fsync(f.fileno())
 os.replace(tmp,r['output'])
 fd=os.open(os.path.dirname(r['output']),os.O_RDONLY);os.fsync(fd);os.close(fd)
 dump=open(r['output'],'rb').read()
 result.update(digest=hashlib.sha256(dump).hexdigest(),size=len(dump))
print(json.dumps(result),flush=True)
`
