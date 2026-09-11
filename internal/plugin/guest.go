package plugin

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/guestjob"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

var guestID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,100}$`)

type Guest struct {
	Client          guestjob.Client
	RunID, Revision string
	// Generation changes only after a proven terminal failure. The plugin's
	// operation ID stays unchanged so it can reconcile external effects.
	Generation int
	// ResolveCredentials is evaluated only when no submitted guest operation
	// exists. Reconnect must not require access to a rotated or removed secret.
	ResolveCredentials func() (map[string]string, error)
}

func (g Guest) directory(b Binding) (string, error) {
	if !guestID.MatchString(g.RunID) || !guestID.MatchString(g.Revision) || !guestID.MatchString(b.Ref.ID) {
		return "", errors.New("invalid plugin guest identity")
	}
	return "/work/envctl/plugins/" + g.Revision + "/" + b.Ref.ID, nil
}

// Install transfers only the digest-locked package, never a host mount. Package
// files are root-owned; the plugin has a separate writable state directory.
func (g Guest) Install(ctx context.Context, b Binding) error {
	dir, err := g.directory(b)
	if err != nil {
		return err
	}
	if digest, e := SourceDigest(b.SourceDir); e != nil || digest != b.Digest {
		return errors.New("plugin package no longer matches its lock")
	}
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	var total int64
	err = filepath.WalkDir(b.SourceDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("plugin package contains a non-regular file")
		}
		total += info.Size()
		if total > 32<<20 {
			return errors.New("plugin package exceeds 32 MiB")
		}
		rel, err := filepath.Rel(b.SourceDir, path)
		if err != nil {
			return err
		}
		if err = tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), Size: info.Size()}); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		return errors.Join(copyErr, f.Close())
	})
	if err != nil {
		return err
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if digest, e := SourceDigest(b.SourceDir); e != nil || digest != b.Digest {
		return errors.New("plugin package changed during transfer preparation")
	}
	sum := sha256.Sum256(raw.Bytes())
	platforms, _ := json.Marshal(b.Descriptor.Platforms)
	err = g.Client.Provider.Exec(ctx, g.Client.Runtime, vm.Command{Args: []string{"sudo", "python3", "-c", installPlugin, dir, b.Digest, hex.EncodeToString(sum[:]), string(platforms)}, Stdin: &raw, Stderr: io.Discard})
	if err != nil {
		return errors.New("guest plugin installation or integrity check failed")
	}
	return nil
}

const installPlugin = `import hashlib,io,json,os,pathlib,pwd,shutil,sys,tarfile,tempfile
root,digest,transport,platforms=sys.argv[1:]
supported=json.loads(platforms)
arch={'aarch64':'arm64','x86_64':'amd64'}.get(os.uname().machine,os.uname().machine)
if supported and 'linux/'+arch not in supported: raise RuntimeError('unsupported plugin platform')
raw=sys.stdin.buffer.read(34*1024*1024)
if hashlib.sha256(raw).hexdigest()!=transport: raise RuntimeError('transfer digest mismatch')
os.makedirs(root,mode=0o755,exist_ok=True)
os.chown(root,0,0);os.chmod(root,0o755)
state=root+'/state'
os.makedirs(state,mode=0o700,exist_ok=True)
agent=pwd.getpwnam('envctl-agent');os.chown(state,agent.pw_uid,agent.pw_gid)
tmp=tempfile.mkdtemp(prefix='.install-',dir=root)
dest=root+'/package'
entries={}
try:
 with tarfile.open(fileobj=io.BytesIO(raw),mode='r:') as archive:
  for member in archive:
   path=pathlib.PurePosixPath(member.name)
   if not member.isfile() or path.is_absolute() or '..' in path.parts or str(path) in entries: raise RuntimeError('invalid package member')
   value=archive.extractfile(member).read()
   mode=0o555 if member.mode & 0o111 else 0o444
   target=tmp+'/'+str(path)
   os.makedirs(os.path.dirname(target),mode=0o755,exist_ok=True)
   with open(target,'xb') as f: f.write(value);f.flush();os.fsync(f.fileno())
   os.chmod(target,mode)
   entries[str(path)]={'sha':hashlib.sha256(value).hexdigest(),'mode':mode,'source_mode':member.mode,'size':len(value)}
 # Reproduce the coordinator source digest from original file modes and bytes.
 h=hashlib.sha256()
 for name,v in sorted(entries.items()):
  h.update((str(len(name.encode()))+':'+name+':'+format(v['source_mode'],'o')+':'+str(v['size'])+':').encode())
  h.update(open(tmp+'/'+name,'rb').read())
 if h.hexdigest()!=digest: raise RuntimeError('source digest mismatch')
 if os.path.exists(dest):
  actual={}
  for base,dirs,files in os.walk(dest,followlinks=False):
   for name in dirs+files:
    if os.path.islink(base+'/'+name): raise RuntimeError('package symlink')
   for name in files:
    p=base+'/'+name;rel=os.path.relpath(p,dest);st=os.stat(p)
    actual[rel]=(hashlib.sha256(open(p,'rb').read()).hexdigest(),st.st_mode & 0o777,st.st_uid)
  expected={name:(v['sha'],v['mode'],0) for name,v in entries.items()}
  if actual!=expected: raise RuntimeError('guest package changed')
 else:
  os.chmod(tmp,0o755);os.rename(tmp,dest)
 receipt=root+'/lock.json'
 lock=json.dumps({'digest':digest,'entries':entries},sort_keys=True).encode()
 if os.path.exists(receipt) and open(receipt,'rb').read()!=lock: raise RuntimeError('lock conflict')
 with open(root+'/.lock.tmp','wb') as f: f.write(lock);f.flush();os.fsync(f.fileno())
 os.chmod(root+'/.lock.tmp',0o444);os.replace(root+'/.lock.tmp',receipt)
 fd=os.open(root,os.O_RDONLY);os.fsync(fd);os.close(fd)
finally:
 if os.path.isdir(tmp): shutil.rmtree(tmp)
`

// Execute reconnects to a systemd-owned operation by stable identity. A lost
// caller never duplicates an in-flight prepare/renew/cleanup effect.
func (g Guest) Execute(ctx context.Context, b Binding, input []byte) ([]byte, []byte, error) {
	if g.Generation < 0 {
		return nil, nil, errors.New("invalid plugin execution generation")
	}
	dir, err := g.directory(b)
	if err != nil {
		return nil, nil, err
	}
	var req Request
	if json.Unmarshal(input, &req) != nil || req.RunID != g.RunID || req.Revision != g.Revision {
		return nil, nil, errors.New("plugin invocation scope mismatch")
	}
	if req.RuntimeID != "" && req.RuntimeID != g.Client.Runtime {
		return nil, nil, errors.New("plugin child runtime scope mismatch")
	}
	if err = g.Install(ctx, b); err != nil {
		return nil, nil, err
	}
	id := "plugin_" + workflow.Digest(struct{ Run, Revision, Plugin, Operation string }{g.RunID, g.Revision, b.Ref.ID, req.OperationID})[:40]
	if g.Generation > 0 {
		id = "plugin_" + workflow.Digest(struct {
			Base       string
			Generation int
		}{id, g.Generation})[:40]
	}
	args := append([]string{"python3", "-c", pluginProcess}, b.Descriptor.Command...)
	timeout := 300
	if req.Operation == "prepare" && b.Descriptor.PrepareSeconds > 0 {
		timeout = b.Descriptor.PrepareSeconds
	}
	if req.TimeoutSeconds > 0 {
		timeout = req.TimeoutSeconds
	}
	job := guestjob.Request{ID: id, Args: args, Dir: dir + "/package", Env: map[string]string{"ENVCTL_PLUGIN_STATE": dir + "/state", "PYTHONDONTWRITEBYTECODE": "1"}, Input: string(input), TimeoutSeconds: timeout}
	exists, err := g.reconnect(ctx, job)
	if err != nil {
		return nil, nil, err
	}
	if !exists {
		if g.ResolveCredentials != nil {
			req.Credentials, err = g.ResolveCredentials()
			if err != nil {
				return nil, nil, err
			}
			input, err = json.Marshal(req)
			if err != nil {
				return nil, nil, err
			}
			job.Input = string(input)
		}
		for _, value := range req.Credentials {
			job.Secrets = append(job.Secrets, value)
		}
		if _, err = g.Client.Submit(ctx, job); err != nil {
			return nil, nil, err
		}
	}
	var output strings.Builder
	var cursor int64
	for {
		status, err := g.Client.Poll(ctx, id, cursor)
		if err != nil {
			return nil, nil, err
		}
		if status.Truncated {
			if terminalJob(status.State) {
				return nil, nil, &TerminalFailure{Detail: "plugin operation exceeded its output bound"}
			}
			return nil, nil, errors.New("plugin operation exceeded its output bound")
		}
		output.WriteString(status.Output)
		cursor = status.Cursor
		if output.Len() > 8<<20 {
			if terminalJob(status.State) {
				return nil, nil, &TerminalFailure{Detail: "plugin operation output exceeds 8 MiB"}
			}
			return nil, nil, errors.New("plugin operation output exceeds 8 MiB")
		}
		if status.Output != "" {
			continue
		}
		if status.State == "completed" {
			var envelope struct {
				Stdout string `json:"stdout"`
				Stderr string `json:"stderr"`
				Exit   int    `json:"exit"`
			}
			if json.Unmarshal([]byte(output.String()), &envelope) != nil {
				return nil, nil, &TerminalFailure{Detail: "invalid plugin execution envelope"}
			}
			if envelope.Exit != 0 {
				return nil, []byte(envelope.Stderr), &TerminalFailure{Detail: "plugin process failed"}
			}
			return []byte(envelope.Stdout), []byte(envelope.Stderr), nil
		}
		if status.State != "running" && status.State != "pending" && status.State != "starting" {
			if terminalJob(status.State) {
				return nil, nil, &TerminalFailure{Detail: "plugin operation ended in " + status.State}
			}
			return nil, nil, errors.New("plugin operation ended in " + status.State)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Reconcile the original root-private request without exporting its secrets.
// Only credential values may differ; command, timeout, scope, configuration,
// operation and input must still match. A transport failure is never absence.
func (g Guest) reconnect(ctx context.Context, job guestjob.Request) (bool, error) {
	var input map[string]any
	if json.Unmarshal([]byte(job.Input), &input) != nil {
		return false, errors.New("invalid plugin reconnect input")
	}
	delete(input, "credentials")
	clean, err := json.Marshal(input)
	if err != nil {
		return false, err
	}
	job.Input, job.Secrets = string(clean), nil
	raw, err := json.Marshal(job)
	if err != nil {
		return false, err
	}
	var out bytes.Buffer
	if err = g.Client.Provider.Exec(ctx, g.Client.Runtime, vm.Command{Args: []string{"sudo", "python3", "-c", reconnectPlugin, guestjob.RunnerPath}, Stdin: bytes.NewReader(raw), Stdout: &out, Stderr: io.Discard}); err != nil {
		return false, errors.New("plugin reconnect transport or binding verification failed")
	}
	switch out.String() {
	case "existing\n":
		return true, nil
	case "missing\n":
		return false, nil
	default:
		return false, errors.New("invalid plugin reconnect response")
	}
}

const reconnectPlugin = `import importlib.util,json,sys
spec=importlib.util.spec_from_file_location('envctl_guest_runner',sys.argv[1])
runner=importlib.util.module_from_spec(spec);spec.loader.exec_module(runner)
candidate=json.load(sys.stdin)
directory=runner.jobdir(candidate['id'])
if not (directory/'receipt.json').exists():
 print('missing')
else:
 original=runner.read(directory/'request.json')
 def identity(value):
  value=dict(value)
  value.pop('secrets',None)
  payload=json.loads(value['input'])
  if not isinstance(payload,dict):raise ValueError('invalid plugin payload')
  payload.pop('credentials',None)
  value['input']=payload
  return value
 if json.dumps(identity(original),sort_keys=True,separators=(',',':'))!=json.dumps(identity(candidate),sort_keys=True,separators=(',',':')):raise ValueError('plugin operation binding changed')
 # submit verifies the original receipt digest and reconciles a durable start
 # intent. It never replaces credentials or reexecutes an interrupted job.
 runner.submit(original)
 print('existing')
`

func terminalJob(state string) bool {
	return state == "completed" || state == "failed" || state == "cancelled" || state == "timed-out"
}

// Keep stderr distinct from the JSON protocol. Redact structured strings before
// encoding, including credentials whose JSON escaping would evade log filters.
const pluginProcess = `import json,os,resource,subprocess,sys,tempfile
resource.setrlimit(resource.RLIMIT_FSIZE,(5*1024*1024,5*1024*1024))
raw=sys.stdin.buffer.read();req=json.loads(raw)
secret=sorted([v for v in req.get('credentials',{}).values() if v],key=len,reverse=True)
def clean(v):
 if isinstance(v,str):
  for s in secret:v=v.replace(s,'[redacted]')
  return v
 if isinstance(v,dict):return {clean(k):clean(x) for k,x in v.items()}
 if isinstance(v,list):return [clean(x) for x in v]
 return v
with tempfile.TemporaryFile() as out,tempfile.TemporaryFile() as err:
 p=subprocess.run(sys.argv[1:],input=raw,stdout=out,stderr=err)
 out.seek(0);err.seek(0);text=out.read(4*1024*1024+1);diag=err.read(1024*1024)
 if len(text)>4*1024*1024:raise RuntimeError('plugin response too large')
 try: stdout=json.dumps(clean(json.loads(text)))
 except Exception: stdout='invalid protocol JSON'
 print(json.dumps({'stdout':stdout,'stderr':clean(diag.decode('utf-8','replace')),'exit':p.returncode}),flush=True)
`
