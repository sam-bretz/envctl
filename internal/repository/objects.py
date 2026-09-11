# Portable companion to a Git bundle: submodule bundles and exact LFS bytes.
# No operation fetches an origin or executes repository hooks.
import hashlib, io, json, os, pathlib, re, subprocess, sys, tarfile, tempfile

LIMIT = 64 * 1024 * 1024
HEX = re.compile(r'^[a-f0-9]{64}$')
SHA = re.compile(r'^[a-f0-9]{40}([a-f0-9]{24})?$')

def git(root, *args, file_transport=False, input=None):
    env = {k:v for k,v in os.environ.items() if not k.startswith('GIT_')}
    env.update(GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL='/dev/null', GIT_TERMINAL_PROMPT='0', GIT_LFS_SKIP_SMUDGE='1')
    result = subprocess.run(['git','-c','core.hooksPath=/dev/null','-c','commit.gpgSign=false','-c','protocol.file.allow='+('always' if file_transport else 'never'),'-C',str(root),*args], env=env, input=input, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if result.returncode: raise RuntimeError('local Git object operation failed: '+args[0])
    return result.stdout

def relative(value, root=False):
    if not isinstance(value,str): raise RuntimeError('invalid source object path')
    if root and value == '.': return value
    p = pathlib.PurePosixPath(value)
    if not value or p.is_absolute() or any(x in ('..','.git','') for x in p.parts) or str(p)!=value or '\x00' in value or value=='.':
        raise RuntimeError('invalid source object path')
    return value

def local(root, value):
    relative(value)
    path = root / value
    # Refuse links in every component, including the final file or directory.
    cursor = root
    for component in pathlib.PurePosixPath(value).parts:
        cursor = cursor / component
        if cursor.is_symlink(): raise RuntimeError('source object path contains a symlink')
    return path

def links(root):
    result = []
    for entry in git(root,'ls-tree','-r','-z','HEAD').split(b'\0'):
        if not entry: continue
        meta,name = entry.split(b'\t',1)
        mode,kind,pin = meta.decode().split(' ')
        if mode=='160000': result.append((relative(name.decode()),pin))
    return result

def lfs(root):
    files = json.loads(git(root,'lfs','ls-files','--include=','--exclude=','--json','HEAD'))['files']
    if files is None: files=[] # Older Git LFS emits null for an empty inventory.
    if not isinstance(files,list): raise RuntimeError('invalid LFS inventory')
    out=[]
    for f in files:
        if f.get('oid_type')!='sha256' or not isinstance(f['oid'],str) or not HEX.fullmatch(f['oid']) or type(f['size']) is not int or f['size']<0:
            raise RuntimeError('unsupported LFS object')
        out.append({'path':relative(f['name']),'oid':f['oid'],'size':f['size']})
    return sorted(out,key=lambda f:f['path'])

def configure_lfs(directory):
    common=pathlib.Path(git(directory,'rev-parse','--path-format=absolute','--git-common-dir').decode().strip())
    media=common/'lfs'
    git(directory,'config','--local','lfs.storage',str(media))
    for key,value in [('filter.lfs.process','git-lfs filter-process'),('filter.lfs.required','true'),('filter.lfs.clean','git-lfs clean -- %f'),('filter.lfs.smudge','git-lfs smudge -- %f')]: git(directory,'config','--local',key,value)
    return media

def collect(root, pin):
    repositories=[]
    def visit(directory, name, expected):
        if len(repositories)>=256: raise RuntimeError('submodule inventory exceeds 256 repositories')
        if git(directory,'rev-parse','HEAD').decode().strip()!=expected: raise RuntimeError('submodule commit differs from parent gitlink')
        objects=lfs(directory)
        if objects: configure_lfs(directory)
        repositories.append((directory, {'path':name,'commit':expected,'lfs':objects}))
        for path,commit in links(directory):
            child=local(directory,path)
            # Git -C on an uninitialized module would discover its parent repo.
            if not (child/'.git').exists(): raise RuntimeError('submodule has not been initialized')
            visit(child, path if name=='.' else name+'/'+path, commit)
        if git(directory,'status','--porcelain','--untracked-files=all','--ignore-submodules=none'): raise RuntimeError('source objects require a clean committed worktree')
    visit(root,'.',pin)
    return repositories

def read_archive(filename, pin):
    if filename.stat().st_size>LIMIT: raise RuntimeError('source objects exceed 64 MiB')
    values={};total=0
    with tarfile.open(filename,'r:') as archive:
        for member in archive:
            if not member.isfile() or member.name in values or (member.name!='manifest.json' and not re.fullmatch(r'payload/[a-f0-9]{64}',member.name)):
                raise RuntimeError('invalid source object archive member')
            total+=member.size
            if member.size<0 or total>LIMIT: raise RuntimeError('source objects exceed 64 MiB')
            value=archive.extractfile(member).read()
            if len(value)!=member.size: raise RuntimeError('truncated source object')
            if member.name!='manifest.json' and hashlib.sha256(value).hexdigest()!=member.name[8:]: raise RuntimeError('source object checksum mismatch')
            values[member.name]=value
    manifest=json.loads(values.pop('manifest.json'))
    if not isinstance(manifest,dict) or set(manifest)!={'version','commit','repositories'} or type(manifest.get('version')) is not int or manifest.get('version')!=1 or manifest.get('commit')!=pin: raise RuntimeError('source object checkpoint binding differs')
    repositories=manifest['repositories']
    if not isinstance(repositories,list) or not repositories or len(repositories)>256: raise RuntimeError('invalid source object inventory')
    paths=set();required=set()
    for repo in repositories:
        if not isinstance(repo,dict): raise RuntimeError('invalid source repository entry')
        name=relative(repo.get('path'),True)
        if set(repo)!=({'path','commit','lfs'} if name=='.' else {'path','commit','lfs','bundle'}): raise RuntimeError('invalid source repository fields')
        if name in paths or not isinstance(repo['commit'],str) or not SHA.fullmatch(repo['commit']): raise RuntimeError('invalid source repository entry')
        paths.add(name)
        if name=='.':
            if repo['commit']!=pin or 'bundle' in repo: raise RuntimeError('root source binding differs')
        else:
            if not isinstance(repo['bundle'],str) or not HEX.fullmatch(repo['bundle']): raise RuntimeError('invalid submodule bundle reference')
            required.add('payload/'+repo['bundle'])
        filenames=set()
        if not isinstance(repo['lfs'],list): raise RuntimeError('invalid LFS inventory')
        for f in repo['lfs']:
            if not isinstance(f,dict) or set(f)!={'path','oid','size'}: raise RuntimeError('invalid LFS entry')
            if relative(f['path']) in filenames or not isinstance(f['oid'],str) or not HEX.fullmatch(f['oid']) or type(f['size']) is not int or f['size']<0: raise RuntimeError('invalid LFS inventory')
            filenames.add(f['path']);required.add('payload/'+f['oid'])
            if len(values.get('payload/'+f['oid'],b''))!=f['size']: raise RuntimeError('LFS size mismatch')
    if '.' not in paths or required!=set(values): raise RuntimeError('source archive has missing or unreferenced payloads')
    return manifest,values

def capture(root,pin,filename):
    repositories=collect(root,pin)
    if filename.exists():
        try:
            manifest,_=read_archive(filename,pin)
            expected=[r for _,r in repositories]
            recorded=[{k:v for k,v in r.items() if k!='bundle'} for r in manifest['repositories']]
            if recorded==expected:return
        except (ValueError,KeyError,TypeError,RuntimeError,tarfile.TarError,EOFError):pass
        # Rebuild a damaged guest cache only from the verified committed source.
    values={};inventory=[];total=0
    def retain(value):
        nonlocal total
        digest=hashlib.sha256(value).hexdigest()
        if digest not in values:
            total+=len(value)
            if total>LIMIT-(1<<20): raise RuntimeError('source objects exceed 64 MiB')
            values[digest]=value
        return digest
    with tempfile.TemporaryDirectory(prefix='envctl-source-') as temp:
        for directory,repo in repositories:
            if repo['path']!='.':
                bundle=pathlib.Path(temp)/'module.bundle'
                git(directory,'bundle','create',str(bundle),'HEAD')
                if bundle.stat().st_size>LIMIT: raise RuntimeError('submodule bundle exceeds 64 MiB')
                repo['bundle']=retain(bundle.read_bytes());bundle.unlink()
            for f in repo['lfs']:
                source=local(directory,f['path'])
                if not source.is_file() or source.stat().st_size!=f['size'] or f['size']>LIMIT: raise RuntimeError('LFS worktree content missing or exceeds limit')
                if retain(source.read_bytes())!=f['oid']: raise RuntimeError('LFS content does not match committed pointer')
            inventory.append(repo)
    manifest={'version':1,'commit':pin,'repositories':inventory}
    filename.parent.mkdir(parents=True,exist_ok=True)
    fd,tmp=tempfile.mkstemp(prefix='.objects-',dir=filename.parent)
    try:
        with os.fdopen(fd,'wb') as out:
            with tarfile.open(fileobj=out,mode='w',format=tarfile.USTAR_FORMAT) as archive:
                entries={'manifest.json':json.dumps(manifest,sort_keys=True,separators=(',',':')).encode()}
                entries.update({'payload/'+key:value for key,value in values.items()})
                for name,value in sorted(entries.items()):
                    info=tarfile.TarInfo(name);info.size=len(value);info.mode=0o600
                    archive.addfile(info,io.BytesIO(value))
            out.flush();os.fsync(out.fileno())
        read_archive(pathlib.Path(tmp),pin)
        os.replace(tmp,filename)
        fd=os.open(filename.parent,os.O_RDONLY);os.fsync(fd);os.close(fd)
    finally:
        if os.path.exists(tmp):os.unlink(tmp)

def atomic_file(target,value,mode,staging):
    target.parent.mkdir(parents=True,exist_ok=True)
    fd,tmp=tempfile.mkstemp(prefix='.hydrate-',dir=staging)
    try:
        with os.fdopen(fd,'wb') as out:
            out.write(value);out.flush();os.fchmod(out.fileno(),mode & 0o777);os.fsync(out.fileno())
        os.replace(tmp,target)
        fd=os.open(target.parent,os.O_RDONLY);os.fsync(fd);os.close(fd)
    finally:
        if os.path.exists(tmp):os.unlink(tmp)

def hydrate(root,pin,filename):
    if git(root,'rev-parse','HEAD').decode().strip()!=pin: raise RuntimeError('assignment source differs')
    if not filename.exists():
        if links(root) or lfs(root): raise RuntimeError('checkpoint has no retained submodule/LFS objects')
        return # Legacy plain-Git checkpoint; its bundle is sufficient.
    manifest,values=read_archive(filename,pin)
    entries={r['path']:r for r in manifest['repositories']};visited=set()
    with tempfile.TemporaryDirectory(prefix='.hydrate-',dir=filename.parent) as temp:
        def visit(directory,name,expected):
            repo=entries.get(name)
            if repo is None or repo['commit']!=expected: raise RuntimeError('submodule inventory differs from committed gitlinks')
            visited.add(name)
            if git(directory,'rev-parse','HEAD').decode().strip()!=expected: raise RuntimeError('hydrated source commit differs')
            git(directory,'diff','--cached','--quiet','--no-ext-diff','HEAD')
            actual=lfs(directory)
            if actual!=repo['lfs']: raise RuntimeError('LFS inventory differs from committed pointers')
            if actual:
                media=configure_lfs(directory)
                for f in actual:
                    value=values['payload/'+f['oid']]
                    target=local(directory,f['path'])
                    existing=target.read_bytes()
                    pointer=git(directory,'show','HEAD:'+f['path'])
                    if existing!=pointer and hashlib.sha256(existing).hexdigest()!=f['oid']: raise RuntimeError('unfinished source has LFS edits')
                    stored=media/'objects'/f['oid'][:2]/f['oid'][2:4]/f['oid']
                    atomic_file(stored,value,0o600,temp)
                    atomic_file(target,value,target.stat().st_mode,temp)
                # Refresh LFS index stat data after replacing pointer-sized files.
                # The cleaned blobs must still match HEAD exactly.
                git(directory,'--literal-pathspecs','add','--pathspec-from-file=-','--pathspec-file-nul',input=b''.join(f['path'].encode()+b'\0' for f in actual))
                git(directory,'diff','--cached','--quiet','--no-ext-diff','HEAD')
            for subpath,commit in links(directory):
                full=subpath if name=='.' else name+'/'+subpath
                child=local(directory,subpath)
                entry=entries.get(full)
                if entry is None or entry['commit']!=commit: raise RuntimeError('submodule bundle missing for committed gitlink')
                if not (child/'.git').exists():
                    if child.exists() and any(child.iterdir()): raise RuntimeError('uninitialized submodule contains unrecorded files')
                    bundle=pathlib.Path(temp)/'module.bundle';bundle.write_bytes(values['payload/'+entry['bundle']])
                    stage=pathlib.Path(temp)/'module-checkout'
                    git(directory,'clone','--no-checkout','--',str(bundle),str(stage),file_transport=True)
                    git(stage,'config','core.hooksPath','/dev/null')
                    git(stage,'config','user.name','envctl worker');git(stage,'config','user.email','envctl@localhost')
                    git(stage,'checkout','--detach',commit)
                    git(stage,'remote','remove','origin')
                    child.parent.mkdir(parents=True,exist_ok=True)
                    os.replace(stage,child)
                    fd=os.open(child.parent,os.O_RDONLY);os.fsync(fd);os.close(fd)
                visit(child,full,commit)
            status=git(directory,'status','--porcelain','--untracked-files=all','--ignore-submodules=none')
            if status: raise RuntimeError('hydrated source is not clean: '+name+' '+status.decode()[:1000])
        visit(root,'.',pin)
    if visited!=set(entries): raise RuntimeError('source inventory contains unrelated submodules')

def commit_work(root,write,message):
    configure_lfs(root)
    if write: git(root,'add','-A')
    children=[]
    if write:
        for entry in git(root,'ls-files','--stage','-z').split(b'\0'):
            if not entry:continue
            meta,name=entry.split(b'\t',1)
            mode,pin,stage=meta.decode().split(' ')
            if stage!='0': raise RuntimeError('source has unresolved merge entries')
            if mode=='160000': children.append((relative(name.decode()),pin))
    else: children=links(root)
    for subpath,pin in children:
        child=local(root,subpath)
        if not (child/'.git').exists(): raise RuntimeError('cannot capture an uninitialized submodule')
        if not write and git(child,'rev-parse','HEAD').decode().strip()!=pin: raise RuntimeError('read-only submodule HEAD changed')
        commit_work(child,write,message)
    if git(root,'status','--porcelain','--untracked-files=all','--ignore-submodules=none'):
        if not write: raise RuntimeError('read-only stage modified repository files')
        git(root,'add','-A');git(root,'commit','-qm',message)
    if git(root,'status','--porcelain','--untracked-files=all','--ignore-submodules=none'): raise RuntimeError('checkpoint source remains dirty')

if __name__=='__main__':
    operation,root,pin,filename=sys.argv[1:]
    root=pathlib.Path(root);filename=pathlib.Path(filename)
    if not SHA.fullmatch(pin): raise RuntimeError('invalid source checkpoint pin')
    if operation=='capture': capture(root,pin,filename)
    elif operation=='hydrate': hydrate(root,pin,filename)
    elif operation=='commit':
        if str(filename)=='readonly' and git(root,'rev-parse','HEAD').decode().strip()!=pin: raise RuntimeError('read-only source HEAD changed')
        commit_work(root,str(filename)=='write','envctl source checkpoint')
        print(git(root,'rev-parse','HEAD').decode().strip())
    elif operation=='restore':
        filename.parent.mkdir(parents=True,exist_ok=True)
        fd,tmp=tempfile.mkstemp(prefix='.restore-',dir=filename.parent)
        try:
            with os.fdopen(fd,'wb') as out:
                raw=sys.stdin.buffer.read(LIMIT+1)
                if len(raw)>LIMIT: raise RuntimeError('source objects exceed 64 MiB')
                out.write(raw);out.flush();os.fsync(out.fileno())
            read_archive(pathlib.Path(tmp),pin)
            # Incoming bytes are checksum-verified retained checkpoint data.
            # They can repair a missing/corrupt guest cache without touching any
            # live assignment's index or files.
            os.replace(tmp,filename)
            fd=os.open(filename.parent,os.O_RDONLY);os.fsync(fd);os.close(fd)
        finally:
            if os.path.exists(tmp):os.unlink(tmp)
    else: raise RuntimeError('unknown source object operation')
