package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	inPath          string
	outPath         string
	mdf             *os.File
	randSource      = rand.NewSource(time.Now().UnixNano())
	reGather        = regexp.MustCompile(`^((Build)?Gather)(.*)`)
	reExample       = regexp.MustCompile(`^(Example)(.*)`)
	reSampleArchive = regexp.MustCompile(`docs/(insights-archive-sample/.*)`)
	cleanRoot       = "./"
)

type DocBlock struct {
	Doc      string
	Examples map[string]string
}

func main() {
	flag.StringVar(&inPath, "in", "gatherers", "Package where to find Gather methods")
	flag.StringVar(&outPath, "out", "gathered-data.md", "File to which MD doc will be generated")

	flag.Parse()
	var err error
	mdf, err = os.Create(outPath)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer mdf.Close()

	md := map[string]*DocBlock{}
	err = walkDir(cleanRoot, md)
	if err != nil {
		fmt.Print(err)
	}
	// second pass will gather Sample...
	err = walkDir(cleanRoot, md)
	if err != nil {
		fmt.Print(err)
	}
	keys := make([]string, 0, len(md))
	for k := range md {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	_, err = mdf.WriteString(
		"# Insights Operator Gathered Data\n\n" +
			"This document is auto-generated by `make docs`, and details what \n" +
			"data Insights Operator (IO) gathers from a cluster, the location \n" +
			"where it is stored in the IO archive, " +
			"and the applicable version(s) of OpenShift. \n" +
			"Data stored in the `conditional/` directory is only \n" +
			"gathered when certain conditions are met.\n\n" +
			"The IO pod operates in the `openshift-insights` namespace and the \n" +
			"archives are packaged as `.tar.gz` files in " +
			"`/var/lib/insights-operator`.\n\n" +
			"***\n\n")
	if err != nil {
		fmt.Print(err)
	}
	for _, k := range keys {
		_, err := fmt.Fprintf(mdf,
			"## %s\n\n"+
				"%s\n\n", k, md[k].Doc)
		if err != nil {
			fmt.Print(err)
		}

		if len(md[k].Examples) > 0 {
			size := 0
			for _, e := range md[k].Examples {
				size = len(e)
			}
			size /= len(md[k].Examples)
			_, err := fmt.Fprintf(mdf,
				"Output raw size: %d\n\n"+
					"### Examples\n\n", size)
			if err != nil {
				fmt.Print(err)
			}
			for n, e := range md[k].Examples {
				_, err := fmt.Fprintf(mdf,
					"#### %s\n"+
						"```%s```\n\n", n, e)
				if err != nil {
					fmt.Print(err)
				}
			}
		}
	}
	fmt.Println("Done")
}

// nolint: gocyclo
func walkDir(cleanRoot string, md map[string]*DocBlock) error {
	expPath := ""
	fset := token.NewFileSet() // positions are relative to fset
	return filepath.Walk(cleanRoot, func(path string, info os.FileInfo, e1 error) error {
		if !info.IsDir() {
			return nil
		}
		if expPath != "" {
			// filter only wanted path under our package
			if !strings.Contains(path, expPath) {
				return nil
			}
		}
		d, err := parser.ParseDir(fset, path, nil, parser.ParseComments)
		if err != nil {
			fmt.Println(err)
			return nil
		}

		for astPackageName, astPackage := range d {
			if astPackageName != inPath && expPath == "" {
				continue
			}
			if expPath == "" && len(astPackage.Files) > 0 {
				firstKey := ""
				for key := range astPackage.Files {
					firstKey = key
					break
				}
				if firstKey != "" {
					expPath = filepath.Dir(firstKey)
				}
			}

			ast.Inspect(astPackage, func(n ast.Node) bool {
				// handle function declarations
				fn, ok := n.(*ast.FuncDecl)
				if ok {
					gatherMethodWithSuff := reGather.ReplaceAllString(fn.Name.Name, "$3")
					_, ok2 := md[gatherMethodWithSuff]
					startsWithGatherOrBuildGather := strings.HasPrefix(fn.Name.Name, "Gather") || strings.HasPrefix(fn.Name.Name, "BuildGather")
					if !ok2 && fn.Name.IsExported() && startsWithGatherOrBuildGather && len(fn.Name.Name) > len("Gather") {
						doc := fn.Doc.Text()
						md[gatherMethodWithSuff] = parseDoc(fn.Name.Name, doc)
						fmt.Printf(fn.Name.Name + "\n")
					}
					// Example methods will have Example prefix, and might have additional case suffix:
					// ExampleMostRecentMetrics_case1, we will remove Example prefix
					exampleMethodWithSuff := reExample.ReplaceAllString(fn.Name.Name, "$2")
					var gatherMethod = ""
					for m := range md {
						if strings.HasPrefix(exampleMethodWithSuff, m) {
							gatherMethod = m
							break
						}
					}

					if gatherMethod != "" && fn.Name.IsExported() && strings.HasPrefix(fn.Name.Name, "Example") && len(fn.Name.Name) > len("Example") {
						// Do not execute same method twice
						_, ok := md[exampleMethodWithSuff].Examples[exampleMethodWithSuff]
						if !ok {
							methodFullpackage := mustGetPackageName(cleanRoot, astPackage)

							output, err := execExampleMethod(methodFullpackage, astPackageName, fn.Name.Name)
							if err != nil {
								fmt.Printf("Error when running Example in package %s method %s\n", methodFullpackage, fn.Name.Name)
								fmt.Println(err)
								fmt.Println(output)
								return true
							}
							if md[exampleMethodWithSuff].Examples == nil {
								md[exampleMethodWithSuff].Examples = map[string]string{}
							}
							md[exampleMethodWithSuff].Examples[exampleMethodWithSuff] = output
						}
						fmt.Printf(fn.Name.Name + "\n")
					}
				}
				return true
			})
		}
		return nil
	})
}

// findGoMod browses the directory tree starting at the given path
// and then going up the tree, looking for the first occurrence go.mod file.
func findGoMod(pkgFilePath string) (goModPath, relPkgPath string, err error) {
	absPkgFilePath, err := filepath.Abs(pkgFilePath)
	if err != nil {
		return "", "", err
	}

	dirPath := absPkgFilePath
	for {
		goModPath = filepath.Join(dirPath, "go.mod")
		if _, err = os.Stat(goModPath); os.IsNotExist(err) {
			// This directory does not contain a go.mod file. Go to the parent directory.
			parentDir := filepath.Dir(dirPath)
			if parentDir == dirPath {
				return "", "", fmt.Errorf("there is no go.mod file in the directory tree of %q", pkgFilePath)
			}
			dirPath = parentDir
			continue
		} else if err != nil {
			return "", "", err
		}

		relPkgPath, err = filepath.Rel(dirPath, absPkgFilePath)
		if err != nil {
			return "", "", err
		}

		// If the go.mod file was found, both paths will be set and the error will be nil.
		return
	}
}

// getModuleNameFromGoMod parses the go.mod file and returns the name (URL) of the module.
func getModuleNameFromGoMod(goModPath string) (string, error) {
	goModBytes, err := os.ReadFile(goModPath)
	if err != nil {
		return "", err
	}

	re := regexp.MustCompile(`module (\S+)`)
	matches := re.FindAllSubmatch(goModBytes, -1)
	if len(matches) != 1 {
		return "", fmt.Errorf("invalid go.mod format; contains %d module names instead of 1", len(matches))
	}

	firstMatch := matches[0]
	if len(firstMatch) != 2 {
		return "", fmt.Errorf("unexpected number of groups captured by regular expression: %d (expected 2)", len(firstMatch))
	}

	return string(firstMatch[1]), nil
}

// mustGetPackageName generates full package name from asp.Package
//
//	astRoot the relative path where ast.Package was parsed from, because ast.Package is relative to astRoot path
//	f ast.Package with containing files
//
// The import path is based on the path of source files in the package and the module name in the nearest go.mod file.
// Exits the program with an error return code in case of an error.
func mustGetPackageName(astRoot string, f *ast.Package) string {
	firstKey := ""
	for key := range f.Files {
		firstKey = key
		break
	}
	if firstKey == "" {
		log.Fatalf("Package %q is composed of %d source files", f.Name, len(f.Files))
	}

	pkgAbs, err := filepath.Abs(filepath.Join(astRoot, filepath.Dir(firstKey)))
	if err != nil {
		log.Fatal(err)
	}
	goModPath, relPkgPath, err := findGoMod(pkgAbs)
	if err != nil {
		log.Fatal(err)
	}
	moduleName, err := getModuleNameFromGoMod(goModPath)
	if err != nil {
		log.Fatal(err)
	}
	importPath := filepath.Join(moduleName, relPkgPath)
	return importPath
}

// execExampleMethod executes the method by starting go run and capturing the produced standard output
func execExampleMethod(methodFullPackage, methodPackage, methodName string) (string, error) {
	f := createRandom()

	tf, err := os.Create(f)
	if err != nil {
		fmt.Println(err)
		return "", err
	}

	defer func() {
		err = os.Remove(f)
		if err != nil {
			fmt.Print(err)
		}
	}()
	_, err = fmt.Fprintf(tf, `package main
	import "%s"
	import "fmt"

	func main() {
		str, _ := %s.%s()
		fmt.Print(str)
	}
		`, methodFullPackage, methodPackage, methodName)
	if err != nil {
		fmt.Print(err)
	}

	// nolint: gosec
	cmd := exec.Command("go", "run", fmt.Sprintf("%s%s", cleanRoot, f))
	output, err := cmd.CombinedOutput()

	return string(output), err
}

// createRandom creates a random non-existing file name in current folder
func createRandom() string {
	var f string
	for {
		f = fmt.Sprintf("sample%d.go", randSource.Int63())
		_, err := os.Stat(f)
		if os.IsNotExist(err) {
			break
		}
	}
	return f
}

func parseDoc(method, doc string) *DocBlock {
	if strings.HasPrefix(doc, method) {
		doc = strings.TrimLeft(doc, method)
	}
	doc = strings.TrimLeft(doc, " ")
	// generates the link to the sample archive
	doc = reSampleArchive.ReplaceAllString(doc, "[$0](./$1)")
	db := &DocBlock{Doc: doc}
	return db
}
