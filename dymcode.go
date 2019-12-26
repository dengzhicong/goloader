package goloader

import (
	"bytes"
	"cmd/objfile/goobj"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

func assert(err error) {
	if err != nil {
		panic(err)
	}
}

// copy from $GOROOT/src/cmd/internal/objabi/reloctype.go
const (
	// R_TLS_LE, used on 386, amd64, and ARM, resolves to the offset of the
	// thread-local symbol from the thread local base and is used to implement the
	// "local exec" model for tls access (r.Sym is not set on intel platforms but is
	// set to a TLS symbol -- runtime.tlsg -- in the linker when externally linking).
	R_TLS_LE    = 16
	R_CALL      = 8
	R_CALLARM   = 9
	R_CALLARM64 = 10
	R_CALLIND   = 11
	R_PCREL     = 15
	R_ADDR      = 1
	// R_ADDRARM64 relocates an adrp, add pair to compute the address of the
	// referenced symbol.
	R_ADDRARM64 = 3
	// R_ADDROFF resolves to a 32-bit offset from the beginning of the section
	// holding the data being relocated to the referenced symbol.
	R_ADDROFF = 5
	// R_WEAKADDROFF resolves just like R_ADDROFF but is a weak relocation.
	// A weak relocation does not make the symbol it refers to reachable,
	// and is only honored by the linker if the symbol is in some other way
	// reachable.
	R_WEAKADDROFF = 6
	// R_METHODOFF resolves to a 32-bit offset from the beginning of the section
	// holding the data being relocated to the referenced symbol.
	// It is a variant of R_ADDROFF used when linking from the uncommonType of a
	// *rtype, and may be set to zero by the linker if it determines the method
	// text is unreachable by the linked program.
	R_METHODOFF = 24
)

// copy from $GOROOT/src/cmd/internal/objabi/symkind.go
const (
	// An otherwise invalid zero value for the type
	Sxxx = iota
	// Executable instructions
	STEXT
	// Read only static data
	SRODATA
	// Static data that does not contain any pointers
	SNOPTRDATA
	// Static data
	SDATA
	// Statically data that is initially all 0s
	SBSS
	// Statically data that is initially all 0s and does not contain pointers
	SNOPTRBSS
	// Thread-local data that is initally all 0s
	STLSBSS
	// Debugging data
	SDWARFINFO
	SDWARFRANGE
)

type SymData struct {
	Name   string
	Kind   int
	Offset int
	Reloc  []Reloc
}

type Reloc struct {
	Offset int
	SymOff int
	Size   int
	Type   int
	Add    int
}

// CodeReloc dispatch and load CodeReloc struct via network is OK
type CodeReloc struct {
	Code []byte
	Data []byte
	Mod  Module
	Syms []SymData
}

type CodeModule struct {
	Syms       map[string]uintptr
	CodeByte   []byte
	Module     interface{}
	pcfuncdata []findfuncbucket
	stkmaps    [][]byte
	itabs      []itabSym
	typemap    map[typeOff]uintptr
}

type itabSym struct {
	Reloc
	ptr   int
	inter int
	_type int
}

type symFile struct {
	sym  *goobj.Sym
	file *os.File
}

type segment struct {
	codeBase   int
	dataBase   int
	codeLen    int
	maxCodeLen int
	offset     int
	symAddrs   []int
	itabMap    map[string]int
	funcType   map[string]*int
	codeByte   []byte
	typeSymPtr map[string]uintptr
	err        bytes.Buffer
}

var (
	modules     = make(map[interface{}]bool)
	modulesLock sync.Mutex
	mov32bit         = [8]byte{0x00, 0x00, 0x80, 0xD2, 0x00, 0x00, 0xA0, 0xF2}
	armcode          = []byte{0x04, 0xF0, 0x1F, 0xE5, 0x00, 0x00, 0x00, 0x00}
	arm64code        = []byte{0x43, 0x00, 0x00, 0x58, 0x60, 0x00, 0x1F, 0xD6, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	x86code          = []byte{0xff, 0x25, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	movcode     byte = 0x8b
	leacode     byte = 0x8d
	cmplcode    byte = 0x83
	jmpcode     byte = 0xe9
)

type objSyms struct {
	symfiles map[string]symFile
	symMap   map[string]int
	gcObjs   map[string]uintptr
	fileMap  map[string]int
}

func readObj(f *os.File, reloc *CodeReloc, objsyms *objSyms, pkgpath *string) error {
	if pkgpath == nil || *pkgpath == "" {
		var defaultPkgPath = "main"
		pkgpath = &defaultPkgPath
	}
	obj, err := goobj.Parse(f, *pkgpath)
	if err != nil {
		return fmt.Errorf("read error: %v", err)
	}
	for _, sym := range obj.Syms {
		objsyms.symfiles[sym.Name] = symFile{
			sym:  sym,
			file: f,
		}
	}
	for _, sym := range obj.Syms {
		if sym.Kind == STEXT && sym.DupOK == false {
			relocSym(reloc, sym.Name, objsyms)
		} else if sym.Kind == SRODATA {
			if strings.HasPrefix(sym.Name, "type.") {
				relocSym(reloc, sym.Name, objsyms)
			}
		}
	}
	return nil
}

func ReadObj(f *os.File) (*CodeReloc, error) {
	var reloc CodeReloc
	var objsyms = objSyms{make(map[string]symFile), make(map[string]int), make(map[string]uintptr), make(map[string]int)}
	return &reloc, readObj(f, &reloc, &objsyms, nil)
}

func ReadObjs(files []string, pkgPath []string) (*CodeReloc, error) {
	var reloc CodeReloc
	var objsyms = objSyms{make(map[string]symFile), make(map[string]int), make(map[string]uintptr), make(map[string]int)}
	for i, file := range files {
		fmt.Println(file, pkgPath[i])
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		err = readObj(f, &reloc, &objsyms, &(pkgPath[i]))
		if err != nil {
			return nil, err
		}
	}
	return &reloc, nil
}

func addSymMap(symMap map[string]int, symArray *[]SymData, rsym *SymData) int {
	var offset int
	if of, ok := symMap[rsym.Name]; !ok {
		offset = len(*symArray)
		*symArray = append(*symArray, *rsym)
		symMap[rsym.Name] = offset
	} else {
		offset = of
		(*symArray)[offset] = *rsym
	}
	return offset
}

type readAtSeeker struct {
	io.ReadSeeker
}

func (r *readAtSeeker) ReadAt(p []byte, offset int64) (n int, err error) {
	_, err = r.Seek(offset, io.SeekStart)
	if err != nil {
		return
	}
	return r.Read(p)
}

func relocSym(reloc *CodeReloc, symName string, objsyms *objSyms) int {
	if offset, ok := objsyms.symMap[symName]; ok {
		return offset
	}
	symfile := objsyms.symfiles[symName]
	var rsym SymData
	rsym.Name = symfile.sym.Name
	rsym.Kind = int(symfile.sym.Kind)
	addSymMap(objsyms.symMap, &reloc.Syms, &rsym)

	code := make([]byte, symfile.sym.Data.Size)
	symfile.file.Seek(symfile.sym.Data.Offset, io.SeekStart)
	_, err := symfile.file.Read(code)
	assert(err)
	switch int(symfile.sym.Kind) {
	case STEXT:
		rsym.Offset = len(reloc.Code)
		reloc.Code = append(reloc.Code, code...)
		readFuncData(&reloc.Mod, symName, objsyms, rsym.Offset)
	default:
		rsym.Offset = len(reloc.Data)
		reloc.Data = append(reloc.Data, code...)
	}
	addSymMap(objsyms.symMap, &reloc.Syms, &rsym)

	for _, re := range symfile.sym.Reloc {
		symOff := -1
		if s, ok := objsyms.symfiles[re.Sym.Name]; ok {
			symOff = relocSym(reloc, s.sym.Name, objsyms)
		} else {
			var exsym SymData
			exsym.Name = re.Sym.Name
			exsym.Offset = -1
			if re.Type == R_TLS_LE {
				exsym.Name = TLSNAME
				exsym.Offset = int(re.Offset)
			}
			if re.Type == R_CALLIND {
				exsym.Offset = 0
				exsym.Name = R_CALLIND_NAME
			}
			if strings.HasPrefix(exsym.Name, "type..importpath.") {
				path := strings.Trim(strings.TrimLeft(exsym.Name, "type..importpath."), ".")
				pathbytes := []byte(path)
				pathbytes = append(pathbytes, 0)
				exsym.Offset = len(reloc.Data)
				reloc.Data = append(reloc.Data, pathbytes...)
			}
			symOff = addSymMap(objsyms.symMap, &reloc.Syms, &exsym)
		}
		rsym.Reloc = append(rsym.Reloc,
			Reloc{Offset: int(re.Offset) + rsym.Offset,
				SymOff: symOff,
				Type:   int(re.Type),
				Size:   int(re.Size),
				Add:    int(re.Add)})
	}
	reloc.Syms[objsyms.symMap[symName]].Reloc = rsym.Reloc

	return objsyms.symMap[symName]
}

func strWrite(buf *bytes.Buffer, str ...string) {
	for _, s := range str {
		buf.WriteString(s)
		if s != "\n" {
			buf.WriteString(" ")
		}
	}
}

func relocADRP(mCode []byte, pc int, symAddr int, symName string) {
	pcPage := pc - pc&0xfff
	lowOff := symAddr & 0xfff
	symPage := symAddr - lowOff
	pageOff := symPage - pcPage
	if pageOff > 0x7FFFFFFF || pageOff < -0x80000000 {
		// fmt.Println("adrp overflow!", symName, symAddr, symAddr < (1<<31))
		movlow := binary.LittleEndian.Uint32(mov32bit[:4])
		movhigh := binary.LittleEndian.Uint32(mov32bit[4:])
		adrp := binary.LittleEndian.Uint32(mCode)
		symAddrUint32 := uint32(symAddr)
		movlow = (((adrp & 0x1f) | movlow) | ((symAddrUint32 & 0xffff) << 5))
		movhigh = (((adrp & 0x1f) | movhigh) | ((symAddrUint32 & 0xffff0000) >> 16 << 5))
		// fmt.Println(adrp, movlow, movhigh)
		binary.LittleEndian.PutUint32(mCode, movlow)
		binary.LittleEndian.PutUint32(mCode[4:], movhigh)
		return
	}
	fmt.Println("pageOff<0:", pageOff < 0)
	// 2bit + 19bit + low(12bit) = 33bit
	pageAnd := (uint32((pageOff>>12)&3) << 29) | (uint32((pageOff>>15)&0x7ffff) << 5)

	adrp := binary.LittleEndian.Uint32(mCode)
	adrp = adrp | pageAnd
	binary.LittleEndian.PutUint32(mCode, adrp)

	lowOff = lowOff << 10
	adrpAdd := binary.LittleEndian.Uint32(mCode[4:])
	adrpAdd = adrpAdd | uint32(lowOff)
	binary.LittleEndian.PutUint32(mCode[4:], adrpAdd)
}

func addSymAddrs(code *CodeReloc, symPtr map[string]uintptr, codeModule *CodeModule, seg *segment) {
	for i, sym := range code.Syms {
		if sym.Offset == -1 {
			if ptr, ok := symPtr[sym.Name]; ok {
				seg.symAddrs[i] = int(ptr)
			} else {
				seg.symAddrs[i] = -1
				strWrite(&seg.err, "unresolve external:", sym.Name, "\n")
			}
		} else if sym.Name == TLSNAME {
			RegTLS(symPtr, sym.Offset)
		} else if sym.Kind == STEXT {
			seg.symAddrs[i] = code.Syms[i].Offset + seg.codeBase
			codeModule.Syms[sym.Name] = uintptr(seg.symAddrs[i])
		} else if strings.HasPrefix(sym.Name, "go.itab") {
			if ptr, ok := symPtr[sym.Name]; ok {
				seg.symAddrs[i] = int(ptr)
			} else {
				seg.itabMap[sym.Name] = i
			}
		} else {
			seg.symAddrs[i] = code.Syms[i].Offset + seg.dataBase

			if strings.HasPrefix(sym.Name, "type.func") {
				seg.funcType[sym.Name] = &seg.symAddrs[i]
			}
			if strings.HasPrefix(sym.Name, "type.") {
				if ptr, ok := symPtr[sym.Name]; ok {
					seg.symAddrs[i] = int(ptr)
				} else {
					seg.typeSymPtr[sym.Name] = (uintptr)(seg.symAddrs[i])
				}
			}
		}
	}
}

func addItab(code *CodeReloc, codeModule *CodeModule, seg *segment) {
	for itabName, itabIndex := range seg.itabMap {
		curSym := code.Syms[itabIndex]
		inter := seg.symAddrs[curSym.Reloc[0].SymOff]
		_type := seg.symAddrs[curSym.Reloc[1].SymOff]
		if inter == -1 || _type == -1 {
			seg.itabMap[itabName] = -1
			continue
		}
		seg.itabMap[itabName] = len(codeModule.itabs)
		codeModule.itabs = append(codeModule.itabs, itabSym{inter: inter, _type: _type})

		addIFaceSubFuncType(seg.funcType, codeModule.typemap, (*interfacetype)(unsafe.Pointer(uintptr(inter))), seg.codeBase)
	}
}

func relocateItab(code *CodeReloc, codeModule *CodeModule, seg *segment) {
	for i := range codeModule.itabs {
		it := &codeModule.itabs[i]
		it.ptr = getitab(it.inter, it._type, false)
		if it.ptr == 0 {
			continue
		}
		switch it.Type {
		case R_PCREL:
			pc := seg.codeBase + it.Offset + it.Size
			offset := it.ptr - pc + it.Add
			if offset > 0x7FFFFFFF || offset < -0x80000000 {
				offset = (seg.codeBase + seg.offset) - pc + it.Add
				binary.LittleEndian.PutUint32(seg.codeByte[it.Offset:], uint32(offset))
				seg.codeByte[it.Offset-2:][0] = movcode
				*(*uintptr)(unsafe.Pointer(&(seg.codeByte[seg.offset:][0]))) = uintptr(it.ptr)
				seg.offset += PtrSize
				continue
			}
			binary.LittleEndian.PutUint32(seg.codeByte[it.Offset:], uint32(offset))
		case R_ADDRARM64:
			relocADRP(seg.codeByte[it.Offset:], seg.codeBase+it.Offset, it.ptr, "unknown")
		}
	}
}

func relocate(code *CodeReloc, symPtr map[string]uintptr, codeModule *CodeModule, seg *segment) {
	for _, curSym := range code.Syms {
		for _, loc := range curSym.Reloc {
			sym := code.Syms[loc.SymOff]
			if seg.symAddrs[loc.SymOff] == -1 {
				continue
			}
			if seg.symAddrs[loc.SymOff] == 0 && strings.HasPrefix(sym.Name, "go.itab") {
				itab := &codeModule.itabs[seg.itabMap[sym.Name]]
				itab.Offset = loc.Offset
				itab.Size = loc.Size
				itab.Type = loc.Type
				itab.Add = loc.Add
				continue
			}

			var offset int
			switch loc.Type {
			case R_TLS_LE:
				binary.LittleEndian.PutUint32(code.Code[loc.Offset:], uint32(symPtr[TLSNAME]))
				continue
			case R_CALL, R_PCREL:
				var relocByte = code.Data
				var addrBase = seg.dataBase
				if curSym.Kind == STEXT {
					addrBase = seg.codeBase
					relocByte = code.Code
				}
				offset = seg.symAddrs[loc.SymOff] - (addrBase + loc.Offset + loc.Size) + loc.Add
				if offset > 0x7fffffff || offset < -0x8000000 {
					if seg.offset+8 > seg.maxCodeLen {
						strWrite(&seg.err, "len overflow", "sym:", sym.Name, "\n")
						continue
					}
					rb := relocByte[loc.Offset-2:]
					if loc.Type == R_CALL {
						offset = (seg.codeBase + seg.offset) - (addrBase + loc.Offset + loc.Size)
						copy(seg.codeByte[seg.offset:], x86code)
						binary.LittleEndian.PutUint32(relocByte[loc.Offset:], uint32(offset))
						if uint64(seg.symAddrs[loc.SymOff]+loc.Add) > 0xFFFFFFFF {
							binary.LittleEndian.PutUint64(seg.codeByte[seg.offset+6:], uint64(seg.symAddrs[loc.SymOff]+loc.Add))
						} else {
							binary.LittleEndian.PutUint32(seg.codeByte[seg.offset+6:], uint32(seg.symAddrs[loc.SymOff]+loc.Add))
						}
						seg.offset += len(x86code)
					} else if rb[0] == leacode || rb[0] == movcode || rb[0] == cmplcode || rb[1] == jmpcode {
						offset = (seg.codeBase + seg.offset) - (addrBase + loc.Offset + loc.Size)
						binary.LittleEndian.PutUint32(relocByte[loc.Offset:], uint32(offset))
						if rb[0] == leacode {
							rb[0] = movcode
						}
						if uint64(seg.symAddrs[loc.SymOff]+loc.Add) > 0xFFFFFFFF {
							binary.LittleEndian.PutUint64(seg.codeByte[seg.offset:], uint64(seg.symAddrs[loc.SymOff]+loc.Add))
							seg.offset += 12
						} else {
							binary.LittleEndian.PutUint32(seg.codeByte[seg.offset:], uint32(seg.symAddrs[loc.SymOff]+loc.Add))
							seg.offset += 8
						}
					} else {
						strWrite(&seg.err, "offset overflow sym:", sym.Name, "\n")
						binary.LittleEndian.PutUint32(relocByte[loc.Offset:], uint32(offset))
					}
					continue
				}
				binary.LittleEndian.PutUint32(relocByte[loc.Offset:], uint32(offset))
			case R_CALLARM, R_CALLARM64:
				var add = loc.Add
				var pcOff = 0
				if loc.Type == R_CALLARM {
					add = loc.Add & 0xffffff
					if add > 256 {
						add = 0
					} else {
						add += 2
					}
					pcOff = 8
				}
				offset = (seg.symAddrs[loc.SymOff] - (seg.codeBase + loc.Offset + pcOff) + add) / 4
				if offset > 0x7FFFFF || offset < -0x800000 {
					if seg.offset+4 > seg.maxCodeLen {
						strWrite(&seg.err, "len overflow", "sym:", sym.Name, "\n")
						continue
					}
					align := seg.offset % 4
					if align != 0 {
						seg.offset += (4 - align)
					}
					offset = (seg.offset - (loc.Offset + pcOff)) / 4
					var v = uint32(offset)
					b := code.Code[loc.Offset:]
					b[0] = byte(v)
					b[1] = byte(v >> 8)
					b[2] = byte(v >> 16)
					var jmpLocOff = 0
					var jmpLen = 0
					if loc.Type == R_CALLARM64 {
						copy(seg.codeByte[seg.offset:], arm64code)
						jmpLen = len(arm64code)
						jmpLocOff = 8
					} else {
						copy(seg.codeByte[seg.offset:], armcode)
						jmpLen = len(armcode)
						jmpLocOff = 4
					}
					*(*uintptr)(unsafe.Pointer(&(seg.codeByte[seg.offset+jmpLocOff:][0]))) = uintptr(seg.symAddrs[loc.SymOff] + add*4)
					seg.offset += jmpLen
					continue
				}
				var v = uint32(offset)
				b := code.Code[loc.Offset:]
				b[0] = byte(v)
				b[1] = byte(v >> 8)
				b[2] = byte(v >> 16)
			case R_ADDRARM64:
				if curSym.Kind != STEXT {
					strWrite(&seg.err, "not in code?\n")
				}
				relocADRP(code.Code[loc.Offset:], seg.codeBase+loc.Offset, seg.symAddrs[loc.SymOff], sym.Name)
			case R_ADDR:
				var relocByte = code.Data
				if curSym.Kind == STEXT {
					relocByte = code.Code
				}
				offset = seg.symAddrs[loc.SymOff] + loc.Add
				*(*uintptr)(unsafe.Pointer(&(relocByte[loc.Offset:][0]))) = uintptr(offset)
			case R_CALLIND:

			case R_ADDROFF, R_WEAKADDROFF, R_METHODOFF:
				var relocByte = code.Data
				var addrBase = seg.codeBase
				if curSym.Kind == STEXT {
					strWrite(&seg.err, "impossible!", sym.Name, "locate on code segment", "\n")
				}
				offset = seg.symAddrs[loc.SymOff] - addrBase + loc.Add
				binary.LittleEndian.PutUint32(relocByte[loc.Offset:], uint32(offset))
			default:
				strWrite(&seg.err, "unknown reloc type:", strconv.Itoa(loc.Type), sym.Name, "\n")
			}

		}
	}
}

func addFuncTab(module *moduledata, i, pclnOff int, code *CodeReloc, seg *segment, symPtr map[string]uintptr) int {
	module.ftab[i].entry = uintptr(seg.symAddrs[int(code.Mod.ftab[i].entry)])

	if pclnOff%PtrSize != 0 {
		pclnOff = pclnOff + (PtrSize - pclnOff%PtrSize)
	}
	module.ftab[i].funcoff = uintptr(pclnOff)
	fi := code.Mod.funcinfo[i]
	fi.entry = module.ftab[i].entry
	copy2Slice(module.pclntable[pclnOff:], unsafe.Pointer(&fi._func), _funcSize)
	pclnOff += _funcSize

	if len(fi.pcdata) > 0 {
		size := int(4 * fi.npcdata)
		copy2Slice(module.pclntable[pclnOff:], unsafe.Pointer(&fi.pcdata[0]), size)
		pclnOff += size
	}

	var funcdata = make([]uintptr, len(fi.funcdata))
	copy(funcdata, fi.funcdata)
	for i, v := range fi.funcdata {
		if v != 0 {
			funcdata[i] = (uintptr)(unsafe.Pointer(&(code.Mod.stkmaps[v][0])))
		} else {
			funcdata[i] = (uintptr)(0)
		}
	}
	if pclnOff%PtrSize != 0 {
		pclnOff = pclnOff + (PtrSize - pclnOff%PtrSize)
	}
	AddStackObject(code, &fi, seg, symPtr)
	funcDataSize := int(PtrSize * fi.nfuncdata)
	copy2Slice(module.pclntable[pclnOff:], unsafe.Pointer(&funcdata[0]), funcDataSize)
	pclnOff += funcDataSize

	return pclnOff
}

func buildModule(code *CodeReloc, symPtr map[string]uintptr, codeModule *CodeModule, seg *segment) {
	var module moduledata
	module.ftab = make([]functab, len(code.Mod.ftab))
	copy(module.ftab, code.Mod.ftab)
	pclnOff := len(code.Mod.pclntable)
	module.pclntable = make([]byte, len(code.Mod.pclntable)+
		(_funcSize+128)*len(code.Mod.ftab))
	copy(module.pclntable, code.Mod.pclntable)
	module.findfunctab = (uintptr)(unsafe.Pointer(&code.Mod.pcfunc[0]))
	module.minpc = (uintptr)(unsafe.Pointer(&seg.codeByte[0]))
	module.maxpc = (uintptr)(unsafe.Pointer(&seg.codeByte[len(code.Code)-1])) + 2
	module.filetab = code.Mod.filetab
	module.typemap = codeModule.typemap
	module.types = uintptr(seg.codeBase)
	module.etypes = uintptr(seg.codeBase + seg.codeLen)
	module.text = uintptr(seg.codeBase)
	module.etext = uintptr(seg.codeBase + len(code.Code))
	codeModule.pcfuncdata = code.Mod.pcfunc // hold reference
	codeModule.stkmaps = code.Mod.stkmaps
	for i := range module.ftab {
		pclnOff = addFuncTab(&module, i, pclnOff, code, seg, symPtr)
	}
	module.pclntable = module.pclntable[:pclnOff]
	module.ftab = append(module.ftab, functab{})
	for i := len(module.ftab) - 1; i > 0; i-- {
		module.ftab[i] = module.ftab[i-1]
	}
	module.ftab = append(module.ftab, functab{})
	module.ftab[0].entry = module.minpc
	module.ftab[len(module.ftab)-1].entry = module.maxpc

	modulesLock.Lock()
	addModule(codeModule, &module)
	modulesLock.Unlock()

	copy(seg.codeByte, code.Code)
	copy(seg.codeByte[len(code.Code):], code.Data)
	codeModule.CodeByte = seg.codeByte
}

func Load(code *CodeReloc, symPtr map[string]uintptr) (*CodeModule, error) {
	var seg segment
	seg.codeLen = len(code.Code) + len(code.Data)
	seg.maxCodeLen = seg.codeLen * 2
	codeByte, err := Mmap(seg.maxCodeLen)
	if err != nil {
		return nil, err
	}
	seg.codeByte = codeByte

	var codeModule = CodeModule{
		Syms:    make(map[string]uintptr),
		typemap: make(map[typeOff]uintptr),
	}

	seg.codeBase = int((*sliceHeader)(unsafe.Pointer(&codeByte)).Data)
	seg.dataBase = seg.codeBase + len(code.Code)
	seg.symAddrs = make([]int, len(code.Syms))
	seg.funcType = make(map[string]*int)
	seg.itabMap = make(map[string]int)
	seg.typeSymPtr = make(map[string]uintptr)
	seg.offset = seg.codeLen

	addSymAddrs(code, symPtr, &codeModule, &seg)
	addItab(code, &codeModule, &seg)
	relocate(code, symPtr, &codeModule, &seg)
	buildModule(code, symPtr, &codeModule, &seg)
	relocateItab(code, &codeModule, &seg)

	if seg.err.Len() > 0 {
		return &codeModule, errors.New(seg.err.String())
	}
	return &codeModule, nil
}

func copy2Slice(dst []byte, src unsafe.Pointer, size int) {
	var s = sliceHeader{
		Data: (uintptr)(src),
		Len:  size,
		Cap:  size,
	}
	copy(dst, *(*[]byte)(unsafe.Pointer(&s)))
}

func (cm *CodeModule) Unload() {
	runtime.GC()
	modulesLock.Lock()
	removeModule(cm.Module)
	modulesLock.Unlock()
	Munmap(cm.CodeByte)
}
