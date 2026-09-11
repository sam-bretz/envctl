package checkpoint

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

//go:embed http_fixture.py
var fixtureSource []byte

const FixtureImage = "python@sha256:7415fbc3c9e4979cc717d92377ab2bc7b2b4a2af1ac03cc52b5f3f88efedaf3a"

func FixtureFiles(contextPath string) map[string][]byte {
	contextJSON, _ := json.Marshal(filepath.ToSlash(contextPath))
	return map[string][]byte{
		"http_fixture.py": append([]byte(nil), fixtureSource...),
		"Dockerfile":      []byte("FROM " + FixtureImage + "\nRUN adduser -D -u 10001 fixture && mkdir /data && chown fixture /data\nCOPY http_fixture.py /opt/envctl/http_fixture.py\nUSER fixture\nCMD [\"python3\",\"/opt/envctl/http_fixture.py\",\"serve\"]\n"),
		"seed.json":       []byte("{\"version\":1,\"records\":{\"tax-rate\":{\"rate\":0.2}}}\n"),
		"compose.yaml":    []byte("services:\n  emulator:\n    build: " + string(contextJSON) + "\n    volumes: [emulator-data:/data]\n    healthcheck:\n      test: [CMD, python3, -c, \"import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/health')\"]\n      interval: 1s\n      timeout: 1s\n      retries: 20\nvolumes: {emulator-data: {}}\n"),
	}
}

// ScaffoldFixture creates a new directory atomically and refuses to replace
// existing files. contextPath is relative to the workflow repository root.
func ScaffoldFixture(destination, contextPath string) error {
	if _, err := os.Lstat(destination); err == nil {
		return errors.New("fixture directory already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".fixture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for name, raw := range FixtureFiles(contextPath) {
		f, err := os.OpenFile(filepath.Join(tmp, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		_, e := f.Write(raw)
		if err = errors.Join(e, f.Sync(), f.Close()); err != nil {
			return err
		}
	}
	if err = os.Rename(tmp, destination); err != nil {
		return err
	}
	dir, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

const fixtureOperation = `import hashlib,json,os,resource,subprocess,sys
resource.setrlimit(resource.RLIMIT_FSIZE,(64*1024*1024,64*1024*1024))
r=json.load(sys.stdin)
command=['docker','--host','unix:///var/run/docker.sock','exec','-i',r['container'],'python3','/opt/envctl/http_fixture.py']
def call(action,body=None):
 p=subprocess.run(command+[action],input=json.dumps(body).encode() if body is not None else None,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
 if p.returncode:raise RuntimeError('fixture operation failed')
 return p.stdout
version=call('version').decode().strip()
if version!='envctl-http-fixture/1.0.0' or (r['version'] and version!=r['version']):raise RuntimeError('fixture version mismatch')
request={'key':r['key'],'equals':json.loads(r['equals'])}
if r['action'] in ['seed','restore']:
 raw=open(r['input'],'rb').read()
 if hashlib.sha256(raw).hexdigest()!=r['input_digest']:raise RuntimeError('fixture input integrity failed')
 request['snapshot']=json.loads(raw)
 call('restore',request)
result={'ok':True,'version':version,'detail':'fixture application verification passed after '+r['action']}
if r['action'] in ['seed','capture']:
 raw=call('snapshot',request)
 tmp=r['output']+'.tmp'
 with open(tmp,'wb') as f:f.write(raw);f.flush();os.fsync(f.fileno())
 os.replace(tmp,r['output'])
 fd=os.open(os.path.dirname(r['output']),os.O_RDONLY);os.fsync(fd);os.close(fd)
 result.update(digest=hashlib.sha256(raw).hexdigest(),size=len(raw))
else:call('probe',request)
print(json.dumps(result),flush=True)
`
