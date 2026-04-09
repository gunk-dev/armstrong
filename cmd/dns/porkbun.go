package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

const porkbunAPI = "https://api.porkbun.com/api/json/v3"

type porkbunClient struct {
	apiKey    string
	secretKey string
	http      *http.Client
}

func newPorkbunClient() (*porkbunClient, error) {
	apiKey := os.Getenv("PORKBUN_API_KEY")
	secretKey := os.Getenv("PORKBUN_SECRET_KEY")
	if apiKey == "" || secretKey == "" {
		return nil, fmt.Errorf("PORKBUN_API_KEY and PORKBUN_SECRET_KEY must be set")
	}
	return &porkbunClient{
		apiKey:    apiKey,
		secretKey: secretKey,
		http:      &http.Client{},
	}, nil
}

type authBody struct {
	APIKey       string `json:"apikey"`
	SecretAPIKey string `json:"secretapikey"`
}

func (c *porkbunClient) auth() authBody {
	return authBody{APIKey: c.apiKey, SecretAPIKey: c.secretKey}
}

type porkbunRecord struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Content  string `json:"content"`
	TTL      string `json:"ttl"`
	Priority string `json:"prio"`
}

type retrieveResponse struct {
	Status  string          `json:"status"`
	Records []porkbunRecord `json:"records"`
}

func (c *porkbunClient) retrieve(domain string) ([]porkbunRecord, error) {
	body, _ := json.Marshal(c.auth())
	resp, err := c.http.Post(porkbunAPI+"/dns/retrieve/"+domain, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("retrieve records: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var result retrieveResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse retrieve response: %w", err)
	}
	if result.Status != "SUCCESS" {
		return nil, fmt.Errorf("retrieve failed: %s", string(data))
	}
	return result.Records, nil
}

type createRequest struct {
	authBody
	Type     string `json:"type"`
	Name     string `json:"name,omitempty"`
	Content  string `json:"content"`
	TTL      string `json:"ttl"`
	Priority string `json:"prio,omitempty"`
}

func (c *porkbunClient) create(domain string, req createRequest) error {
	req.authBody = c.auth()
	body, _ := json.Marshal(req)
	resp, err := c.http.Post(porkbunAPI+"/dns/create/"+domain, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create record: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var result struct{ Status string }
	json.Unmarshal(data, &result)
	if result.Status != "SUCCESS" {
		return fmt.Errorf("create failed: %s", string(data))
	}
	return nil
}

type editRequest struct {
	authBody
	Content  string `json:"content"`
	TTL      string `json:"ttl"`
	Priority string `json:"prio,omitempty"`
}

func (c *porkbunClient) editByNameType(domain, recordType, subdomain string, req editRequest) error {
	req.authBody = c.auth()
	body, _ := json.Marshal(req)
	url := fmt.Sprintf("%s/dns/editByNameType/%s/%s/%s", porkbunAPI, domain, recordType, subdomain)
	resp, err := c.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("edit record: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var result struct{ Status string }
	json.Unmarshal(data, &result)
	if result.Status != "SUCCESS" {
		return fmt.Errorf("edit failed: %s", string(data))
	}
	return nil
}

func (c *porkbunClient) deleteByID(domain, id string) error {
	body, _ := json.Marshal(c.auth())
	url := fmt.Sprintf("%s/dns/delete/%s/%s", porkbunAPI, domain, id)
	resp, err := c.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("delete record: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var result struct{ Status string }
	json.Unmarshal(data, &result)
	if result.Status != "SUCCESS" {
		return fmt.Errorf("delete failed: %s", string(data))
	}
	return nil
}

func (c *porkbunClient) deleteByNameType(domain, recordType, subdomain string) error {
	body, _ := json.Marshal(c.auth())
	url := fmt.Sprintf("%s/dns/deleteByNameType/%s/%s/%s", porkbunAPI, domain, recordType, subdomain)
	resp, err := c.http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("delete record: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	var result struct{ Status string }
	json.Unmarshal(data, &result)
	if result.Status != "SUCCESS" {
		return fmt.Errorf("delete failed: %s", string(data))
	}
	return nil
}
