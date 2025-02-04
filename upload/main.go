package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var (
	githubOrg   string
	githubRepo  string
	githubPat   string
	archivePath string
)

var rootCmd = &cobra.Command{
	Use:   "migrate-repo",
	Short: "Migrate a Bitbucket Server repository to GitHub",
	Run: func(cmd *cobra.Command, args []string) {

		// get org ID for repo creation
		orgId, _ := getOrgID(githubOrg, githubPat)

		fmt.Println("Creating repository...")

		createRepoMutation := `
    mutation($org: ID!, $name: String!, $visibility: RepositoryVisibility!) {
        createRepository(input: {ownerId: $org, name: $name, visibility: $visibility}) {
            repository {
                id
                name
                url
            }
        }
    }
`

		variables := map[string]interface{}{
			"org":        orgId,
			"name":       githubRepo,
			"visibility": "PRIVATE", // or "PUBLIC"
		}

		payload := map[string]interface{}{
			"query":     createRepoMutation,
			"variables": variables,
		}

		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			log.Fatalf("Failed to marshal payload: %v", err)
		}

		req, err := http.NewRequest("POST", "https://api.github.com/graphql", bytes.NewBuffer(payloadBytes))
		if err != nil {
			log.Fatalf("Failed to create request: %v", err)
		}

		req.Header.Set("Authorization", "Bearer "+githubPat)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			log.Fatalf("Failed to send request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusConflict {
			log.Fatalf("Repository name already exists on this account.")
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Fatalf("Failed to read response body: %v", err)
		}

		fmt.Println(string(body))

		fmt.Println("Starting migration...")

		// Extract the tar archive
		fmt.Printf("Extracting tar archive from %s to ./extracted\n", archivePath)
		err = extractTarArchive(archivePath, "./extracted")
		if err != nil {
			log.Fatalf("Failed to extract tar archive: %v", err)
		}

		// Upload the extracted files to the new repository
		err = uploadFilesToRepo(githubOrg, githubRepo, githubPat, "./extracted")
		if err != nil {
			log.Fatalf("Failed to upload files to repository: %v", err)
		}

		fmt.Println("Migration completed successfully")
	},
}

func getOrgID(orgName, githubPat string) (string, error) {
	orgQuery := `
    query($login: String!) {
        organization(login: $login) {
            id
        }
    }
`

	orgVariables := map[string]interface{}{
		"login": orgName,
	}

	orgPayload := map[string]interface{}{
		"query":     orgQuery,
		"variables": orgVariables,
	}

	orgPayloadBytes, err := json.Marshal(orgPayload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal payload: %v", err)
	}

	orgReq, err := http.NewRequest("POST", "https://api.github.com/graphql", bytes.NewBuffer(orgPayloadBytes))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}

	orgReq.Header.Set("Authorization", "Bearer "+githubPat)
	orgReq.Header.Set("Content-Type", "application/json")

	orgClient := &http.Client{}
	orgResp, err := orgClient.Do(orgReq)
	if err != nil {
		return "", fmt.Errorf("failed to send request: %v", err)
	}
	defer orgResp.Body.Close()

	orgBody, err := io.ReadAll(orgResp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}

	var orgRespData struct {
		Data struct {
			Organization struct {
				ID string `json:"id"`
			} `json:"organization"`
		} `json:"data"`
	}

	err = json.Unmarshal(orgBody, &orgRespData)
	if err != nil {
		return "", fmt.Errorf("failed to unmarshal response: %v", err)
	}

	return orgRespData.Data.Organization.ID, nil
}

func extractTarArchive(src, dest string) error {
	if err := os.RemoveAll(dest); err != nil {
		return err
	}

	// Create the destination directory if it doesn't exist
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}

	file, err := os.Open(src)
	if err != nil {
		return err
	}
	defer file.Close()

	var tarReader *tar.Reader
	if strings.HasSuffix(src, ".gz") {
		gzr, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer gzr.Close()
		tarReader = tar.NewReader(gzr)
	} else {
		tarReader = tar.NewReader(file)
	}

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Strip the top-level directory component
		relativePath := header.Name
		if idx := strings.Index(relativePath, "/"); idx != -1 {
			relativePath = relativePath[idx+1:]
		}

		// Skip empty paths (which can happen if the top-level directory is stripped)
		if relativePath == "" {
			continue
		}

		target := filepath.Join(dest, relativePath)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		case tar.TypeXGlobalHeader:
			// Ignore the global header
		default:
			fmt.Printf("Unable to untar type: %c in file %s\n", header.Typeflag, header.Name)
		}
	}
	return nil
}

func uploadFilesToRepo(org, repo, pat, srcDir string) error {
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		relativePath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		fileContent, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		encodedContent := base64.StdEncoding.EncodeToString(fileContent)

		payload := map[string]interface{}{
			"message": "Initial commit",
			"content": encodedContent,
			"path":    relativePath,
		}

		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return err
		}

		url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", org, repo, relativePath)
		req, err := http.NewRequest("PUT", url, bytes.NewBuffer(payloadBytes))
		if err != nil {
			return err
		}

		req.Header.Set("Authorization", "Bearer "+pat)
		req.Header.Set("Content-Type", "application/json")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("failed to upload file: %s", body)
		}

		return nil
	})

	return err
}

func init() {
	rootCmd.PersistentFlags().StringVar(&githubOrg, "github-org", "", "The GitHub organization to migrate to")
	rootCmd.PersistentFlags().StringVar(&githubRepo, "github-repo", "", "The GitHub repository to migrate to")
	rootCmd.PersistentFlags().StringVar(&githubPat, "github-pat", "", "The GitHub personal access token to be used for the migration")
	rootCmd.PersistentFlags().StringVar(&archivePath, "archive-path", "", "The path to the Bitbucket Server repository archive")

	rootCmd.MarkPersistentFlagRequired("github-org")
	rootCmd.MarkPersistentFlagRequired("github-repo")
	rootCmd.MarkPersistentFlagRequired("github-pat")
	rootCmd.MarkPersistentFlagRequired("archive-path")
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
