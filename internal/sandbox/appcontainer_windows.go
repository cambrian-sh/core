//go:build windows

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows AppContainer-class agent runner (ADR-0157 D2.4, stage S5).
//
// A low-integrity token confines writes, never reads, so it cannot hide the
// kernel's state (configs/, the key file, the data directories). An
// AppContainer can: its access check passes only where an object's DACL grants
// the container's package SID, one of its capability SIDs or ALL APPLICATION
// PACKAGES — and nothing in the user's profile or the repository grants any of
// those. So the agent reads system directories and exactly the paths the kernel
// GRANTED it; everything else, the D1 hide-list included, is unreadable.
//
// How it is built (no admin, nothing left in the registry):
//   - the token is NtCreateLowBoxToken over the kernel's own token (Chromium's
//     lowbox path): a derived package SID per INSTANCE plus capability SIDs.
//     exec.Cmd launches it through CreateProcessAsUser (SysProcAttr.Token), so
//     the Job Object, the suspended start and the pipes are the native runner's
//     unchanged. No AppContainer profile is created — the package SID is only
//     derived from a name — so there is nothing to delete afterwards;
//   - reads: the kernel-known runtime paths (the agent's code, the interpreter
//     and its base installation, agents/, prompts/, skills/) are granted to ONE
//     shared capability SID (cambrianAgentRuntime) that every agent token holds;
//     a manifest-declared path is granted to that agent's own capability SID
//     (cambrianAgent.<agent id>), so no other agent can reach it;
//   - writes: the instance's scratch (TEMP/HOME/out/) and its socket directory
//     are granted to the instance's package SID (the ACE goes with the
//     directory at exit) and labelled low integrity, as before;
//   - network (D3): no network capability ⇒ no network at all, loopback
//     included — the agent reaches the kernel only through the unix kernel
//     socket in its socket directory. A tier with the normal network gets
//     internetClient, internetClientServer and privateNetworkClientServer;
//     loopback stays blocked for every AppContainer (Windows loopback
//     isolation), so that tier reaches the kernel through the socket too.
//
// A grant is an ACE added to the path's DACL (inheritable), plus an explicit
// ACE on every descendant whose DACL is PROTECTED (uv and pip hardlinks keep
// their cache's protected DACL, which an inherited ACE never reaches). It needs
// WRITE_DAC on the path: a system-wide interpreter owned by Administrators
// (C:\PythonXY) cannot be granted by a non-admin kernel. Then the AppContainer
// is unusable for that agent and the runner falls back to the low-integrity
// token, reporting hide_state and network MISSING with the path and the
// one-time administrator command named.
//
// The report's hide_state is not assumed: every existing hide path (and up to
// hideCheckChildren of a directory's entries) is checked with AccessCheck
// against the real token, and a readable one is named as missing.

// Well-known capability SIDs (the network capabilities).
const (
	capInternetClient             = "S-1-15-3-1"
	capInternetClientServer       = "S-1-15-3-2"
	capPrivateNetworkClientServer = "S-1-15-3-3"
)

// runtimeCapabilityName is the capability every agent token holds and every
// kernel-known runtime path is granted to.
const runtimeCapabilityName = "cambrianAgentRuntime"

// hideCheckChildren bounds how many entries of a hidden directory are checked.
const hideCheckChildren = 64

// Access masks (winnt.h).
const (
	fileReadData       = 0x0001
	fileGenericRead    = 0x00120089
	fileGenericWrite   = 0x00120116
	fileGenericExecute = 0x001200A0
	fileAllAccess      = 0x001F01FF
	accessRead         = windows.GENERIC_READ | windows.GENERIC_EXECUTE
	accessModify       = windows.GENERIC_READ | windows.GENERIC_WRITE | windows.GENERIC_EXECUTE | windows.DELETE
)

var (
	modntdll                       = windows.NewLazySystemDLL("ntdll.dll")
	procNtCreateLowBoxToken        = modntdll.NewProc("NtCreateLowBoxToken")
	moduserenv                     = windows.NewLazySystemDLL("userenv.dll")
	procDeriveAppContainerSid      = moduserenv.NewProc("DeriveAppContainerSidFromAppContainerName")
	modkernelbase                  = windows.NewLazySystemDLL("kernelbase.dll")
	procDeriveCapabilitySidsFromNm = modkernelbase.NewProc("DeriveCapabilitySidsFromName")
	modadvapi32                    = windows.NewLazySystemDLL("advapi32.dll")
	procAccessCheck                = modadvapi32.NewProc("AccessCheck")
)

// appContainer is one instance's lowbox token and the SIDs it carries.
type appContainer struct {
	token    windows.Token
	pkg      *windows.SID // the instance's package SID
	runtime  *windows.SID // shared runtime capability
	agent    *windows.SID // the agent's own capability
	network  bool         // network capabilities granted
	pkgName  string
	identity string
}

func (a *appContainer) close() {
	if a != nil && a.token != 0 {
		_ = a.token.Close()
		a.token = 0
	}
}

// acPlan is what the AppContainer can deliver for one agent spec right now.
type acPlan struct {
	ok       bool
	why      string   // ok == false: why the runner falls back
	readable []string // hide paths the container CAN read (hide_state missing)
	notes    []string
}

// wantsAppContainer: agents only (an MCP server keeps the low-integrity runner
// until T-20261106-windows-appcontainer-runner-for-native-network opts it in),
// and only when the tier wants hidden state or a denied network.
func wantsAppContainer(spec Spec, tp TierPolicy) bool {
	return spec.Agent != nil && (tp.HideState || (tp.DenyNetwork && spec.Server.Network != NetworkEgress))
}

// agentIdentity is the stable name an agent's own grants are scoped to.
func agentIdentity(spec Spec) string {
	if spec.Agent != nil && spec.Agent.Identity != "" {
		return spec.Agent.Identity
	}
	return spec.ServerID
}

// containerName maps a name onto an AppContainer/capability name (at most 64
// characters, [A-Za-z0-9._-]); a longer one keeps a readable prefix and a digest.
func containerName(prefix, id string) string {
	n := prefix + safeName(id)
	if len(n) <= 64 {
		return n
	}
	sum := sha256.Sum256([]byte(id))
	d := hex.EncodeToString(sum[:])[:12]
	return n[:64-len(d)-1] + "-" + d
}

// newAppContainer builds an instance's lowbox token: its package SID, the
// shared runtime capability, the agent's own capability and, when network is
// true, the network capabilities.
func newAppContainer(spec Spec, network bool) (*appContainer, error) {
	ac := &appContainer{network: network, identity: agentIdentity(spec)}
	ac.pkgName = containerName("cambrian.agent.", spec.ServerID)
	var err error
	if ac.pkg, err = derivePackageSID(ac.pkgName); err != nil {
		return nil, fmt.Errorf("appcontainer package sid: %w", err)
	}
	if ac.runtime, err = deriveCapabilitySID(runtimeCapabilityName); err != nil {
		return nil, fmt.Errorf("appcontainer runtime capability: %w", err)
	}
	if ac.agent, err = deriveCapabilitySID(containerName("cambrianAgent.", ac.identity)); err != nil {
		return nil, fmt.Errorf("appcontainer agent capability: %w", err)
	}
	caps := []windows.SIDAndAttributes{
		{Sid: ac.runtime, Attributes: windows.SE_GROUP_ENABLED},
		{Sid: ac.agent, Attributes: windows.SE_GROUP_ENABLED},
	}
	if network {
		for _, s := range []string{capInternetClient, capInternetClientServer, capPrivateNetworkClientServer} {
			sid, serr := windows.StringToSid(s)
			if serr != nil {
				return nil, serr
			}
			caps = append(caps, windows.SIDAndAttributes{Sid: sid, Attributes: windows.SE_GROUP_ENABLED})
		}
	}
	if ac.token, err = lowBoxToken(ac.pkg, caps); err != nil {
		return nil, err
	}
	return ac, nil
}

// objectAttributes is OBJECT_ATTRIBUTES with no name.
type objectAttributes struct {
	Length                   uint32
	RootDirectory            windows.Handle
	ObjectName               uintptr
	Attributes               uint32
	SecurityDescriptor       uintptr
	SecurityQualityOfService uintptr
}

// lowBoxToken is NtCreateLowBoxToken over the kernel's own primary token.
func lowBoxToken(pkg *windows.SID, caps []windows.SIDAndAttributes) (windows.Token, error) {
	if err := procNtCreateLowBoxToken.Find(); err != nil {
		return 0, fmt.Errorf("NtCreateLowBoxToken unavailable: %w", err)
	}
	var cur windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_ADJUST_DEFAULT, &cur); err != nil {
		return 0, err
	}
	defer func() { _ = cur.Close() }()
	var lb windows.Token
	oa := objectAttributes{Length: uint32(unsafe.Sizeof(objectAttributes{}))}
	var capPtr uintptr
	if len(caps) > 0 {
		capPtr = uintptr(unsafe.Pointer(&caps[0]))
	}
	st, _, _ := procNtCreateLowBoxToken.Call(uintptr(unsafe.Pointer(&lb)), uintptr(cur),
		uintptr(windows.TOKEN_ALL_ACCESS), uintptr(unsafe.Pointer(&oa)), uintptr(unsafe.Pointer(pkg)),
		uintptr(len(caps)), capPtr, 0, 0)
	if st != 0 {
		return 0, fmt.Errorf("NtCreateLowBoxToken: NTSTATUS %#x", st)
	}
	return lb, nil
}

func derivePackageSID(name string) (*windows.SID, error) {
	if err := procDeriveAppContainerSid.Find(); err != nil {
		return nil, err
	}
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var sid *windows.SID
	if hr, _, _ := procDeriveAppContainerSid.Call(uintptr(unsafe.Pointer(n)), uintptr(unsafe.Pointer(&sid))); hr != 0 {
		return nil, fmt.Errorf("DeriveAppContainerSidFromAppContainerName: HRESULT %#x", hr)
	}
	defer func() { _ = windows.FreeSid(sid) }()
	return sid.Copy()
}

func deriveCapabilitySID(name string) (*windows.SID, error) {
	if err := procDeriveCapabilitySidsFromNm.Find(); err != nil {
		return nil, err
	}
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var groups, caps **windows.SID
	var ng, nc uint32
	r, _, e := procDeriveCapabilitySidsFromNm.Call(uintptr(unsafe.Pointer(n)),
		uintptr(unsafe.Pointer(&groups)), uintptr(unsafe.Pointer(&ng)),
		uintptr(unsafe.Pointer(&caps)), uintptr(unsafe.Pointer(&nc)))
	if r == 0 {
		return nil, fmt.Errorf("DeriveCapabilitySidsFromName(%s): %w", name, e)
	}
	// Both arrays and every SID in them are LocalAlloc'ed by the API.
	free := func(arr **windows.SID, count uint32) {
		if arr == nil {
			return
		}
		for _, sid := range unsafe.Slice(arr, count) {
			_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(sid)))
		}
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(arr)))
	}
	defer free(groups, ng)
	defer free(caps, nc)
	if nc == 0 || caps == nil {
		return nil, errors.New("DeriveCapabilitySidsFromName returned no capability sid")
	}
	return unsafe.Slice(caps, nc)[0].Copy()
}

// grantMemo remembers the grants this process already made (path|sid|mask):
// a grant is idempotent but walks the tree, so it is done once per kernel run.
var grantMemo struct {
	sync.Mutex
	m map[string]error
}

// ensureGrant adds an inheritable ACE granting mask to sid on path, and the
// same ACE explicitly on every descendant whose DACL is protected (once per
// process per path, sid and mask).
func ensureGrant(path string, sid *windows.SID, mask windows.ACCESS_MASK) error {
	key := strings.ToLower(path) + "|" + sid.String() + "|" + fmt.Sprint(mask)
	grantMemo.Lock()
	defer grantMemo.Unlock()
	if err, ok := grantMemo.m[key]; ok {
		return err
	}
	err := grantTree(path, sid, mask)
	if grantMemo.m == nil {
		grantMemo.m = map[string]error{}
	}
	grantMemo.m[key] = err
	return err
}

func grantTree(path string, sid *windows.SID, mask windows.ACCESS_MASK) error {
	if err := grantOne(path, sid, mask, true); err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		return err
	}
	var failed []string
	walkErr := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == path {
			return nil //nolint:nilerr // an unreadable subtree is reported by the agent's own failure
		}
		protected, has, qerr := daclState(p, sid, mask)
		if qerr != nil || !protected || has {
			return nil //nolint:nilerr // inherits the root's ACE, or already granted
		}
		if gerr := grantOne(p, sid, mask, d.IsDir()); gerr != nil && len(failed) < 3 {
			failed = append(failed, p)
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if len(failed) > 0 {
		return fmt.Errorf("protected descendants could not be granted (first: %s)", strings.Join(failed, ", "))
	}
	return nil
}

// grantOne adds the ACE to one object's DACL unless an equal or wider allow
// ACE for sid is already there.
func grantOne(path string, sid *windows.SID, mask windows.ACCESS_MASK, inherit bool) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if hasAllowACE(dacl, sid, mask) {
		return nil
	}
	inh := uint32(windows.NO_INHERITANCE)
	if inherit {
		inh = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	nd, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: mask, AccessMode: windows.GRANT_ACCESS, Inheritance: inh,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(sid)},
	}}, dacl)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, nd, nil)
}

// daclState reports whether path's DACL is protected and whether it already
// allows mask to sid.
func daclState(path string, sid *windows.SID, mask windows.ACCESS_MASK) (protected, has bool, err error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, false, err
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return false, false, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, false, err
	}
	return ctl&windows.SE_DACL_PROTECTED != 0, hasAllowACE(dacl, sid, mask), nil
}

// hasAllowACE reports whether dacl holds an allow ACE for sid covering mask
// (generic rights mapped to the file rights they stand for).
func hasAllowACE(dacl *windows.ACL, sid *windows.SID, mask windows.ACCESS_MASK) bool {
	if dacl == nil {
		return false
	}
	want := mapGeneric(mask)
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		if (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) && mapGeneric(ace.Mask)&want == want {
			return true
		}
	}
	return false
}

func mapGeneric(m windows.ACCESS_MASK) windows.ACCESS_MASK {
	out := m &^ (windows.GENERIC_READ | windows.GENERIC_WRITE | windows.GENERIC_EXECUTE | windows.GENERIC_ALL)
	if m&windows.GENERIC_READ != 0 {
		out |= fileGenericRead
	}
	if m&windows.GENERIC_WRITE != 0 {
		out |= fileGenericWrite
	}
	if m&windows.GENERIC_EXECUTE != 0 {
		out |= fileGenericExecute
	}
	if m&windows.GENERIC_ALL != 0 {
		out |= fileAllAccess
	}
	return out
}

// genericMapping is GENERIC_MAPPING for files.
type genericMapping struct{ read, write, execute, all uint32 }

// canRead reports whether tok may read path's contents (FILE_READ_DATA /
// FILE_LIST_DIRECTORY), by AccessCheck against the object's security
// descriptor (the AppContainer and integrity checks included).
func canRead(tok windows.Token, path string) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	var imp windows.Token
	if err := windows.DuplicateTokenEx(tok, windows.TOKEN_QUERY, nil, windows.SecurityIdentification,
		windows.TokenImpersonation, &imp); err != nil {
		return false, err
	}
	defer func() { _ = imp.Close() }()
	mapping := genericMapping{fileGenericRead, fileGenericWrite, fileGenericExecute, fileAllAccess}
	var privs [256]byte
	plen := uint32(len(privs))
	var granted, status uint32
	r, _, e := procAccessCheck.Call(uintptr(unsafe.Pointer(sd)), uintptr(imp), fileReadData,
		uintptr(unsafe.Pointer(&mapping)), uintptr(unsafe.Pointer(&privs[0])), uintptr(unsafe.Pointer(&plen)),
		uintptr(unsafe.Pointer(&granted)), uintptr(unsafe.Pointer(&status)))
	if r == 0 {
		return false, fmt.Errorf("AccessCheck %s: %w", path, e)
	}
	return status != 0, nil
}

// readableHidden lists the hide paths (and up to hideCheckChildren entries of
// each hidden directory) the token can read. An entry inside a bind is the
// agent's own re-exposed path (its code inside the kernel directory) and is
// not checked.
func readableHidden(tok windows.Token, hide []string, binds []Bind) []string {
	var out []string
	check := func(p string) {
		for _, b := range binds {
			if b.Path != "" && within(p, b.Path) {
				return
			}
		}
		if ok, err := canRead(tok, p); err == nil && ok {
			out = append(out, p)
		}
	}
	for _, h := range hide {
		fi, err := os.Stat(h)
		if err != nil {
			continue
		}
		check(h)
		if !fi.IsDir() {
			continue
		}
		ents, err := os.ReadDir(h)
		if err != nil {
			continue
		}
		for i, e := range ents {
			if i >= hideCheckChildren {
				break
			}
			check(filepath.Join(h, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// containsHidden returns the first hide path inside dir ("" if none).
func containsHidden(dir string, hide []string) string {
	for _, h := range hide {
		if h != "" && within(h, dir) {
			return h
		}
	}
	return ""
}

// grantTarget resolves a bind to the directory (or file) to grant: a junction
// or symlink is granted at its target (a venv's base interpreter link).
func grantTarget(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// grantBinds grants every bind to the container's SIDs: kernel-known paths to
// the shared runtime capability, declared (Private) paths to the agent's own
// capability; writable ones with modify rights. A bind that would expose a
// hidden path is skipped and named; a bind that cannot be granted makes the
// AppContainer unusable for this agent (the error names it).
func (a *appContainer) grantBinds(binds []Bind, hide []string) (notes []string, err error) {
	for _, b := range binds {
		if b.Path == "" {
			continue
		}
		if _, serr := os.Stat(b.Path); serr != nil {
			notes = append(notes, "bind "+b.Path+" skipped: it does not exist")
			continue
		}
		target := grantTarget(b.Path)
		if h := containsHidden(target, hide); h != "" {
			notes = append(notes, "bind "+b.Path+" not granted: it contains kernel state ("+h+")")
			continue
		}
		sid, mask := a.runtime, windows.ACCESS_MASK(accessRead)
		if b.Private || b.Write {
			sid = a.agent
		}
		if b.Write {
			mask = accessModify
		}
		if gerr := ensureGrant(target, sid, mask); gerr != nil {
			return notes, fmt.Errorf("cannot grant the agent container %s on %s (%v); an administrator can grant it once: icacls \"%s\" /grant *%s:(OI)(CI)%s /T",
				map[bool]string{true: "write", false: "read"}[b.Write], target, gerr, target, sid.String(),
				map[bool]string{true: "M", false: "RX"}[b.Write])
		}
	}
	return notes, nil
}

// grantInstanceDir gives the instance's package SID full control of one of its
// own directories (scratch, socket directory): the ACE is inherited by what
// the agent creates and goes with the directory at exit.
func (a *appContainer) grantInstanceDir(dir string) error {
	return grantOne(dir, a.pkg, windows.GENERIC_ALL, true)
}

// appContainerFor builds the container for spec and prepares its grants;
// checkHidden verifies the hide-list with AccessCheck (the plan does, the
// start that follows it does not repeat it). ok
// false ⇒ the native runner falls back to the low-integrity token, and why
// says what made the AppContainer unusable. The caller closes the returned
// container (nil when !ok).
func appContainerFor(p *Policy, spec Spec, tp TierPolicy, checkHidden bool) (*appContainer, acPlan) {
	network := !(tp.DenyNetwork && spec.Server.Network != NetworkEgress)
	ac, err := newAppContainer(spec, network)
	if err != nil {
		return nil, acPlan{why: err.Error()}
	}
	notes, err := ac.grantBinds(spec.Agent.Binds, p.HidePaths)
	if err != nil {
		ac.close()
		return nil, acPlan{why: err.Error(), notes: notes}
	}
	pl := acPlan{ok: true, notes: notes}
	if tp.HideState && checkHidden {
		pl.readable = readableHidden(ac.token, p.HidePaths, spec.Agent.Binds)
	}
	return ac, pl
}
