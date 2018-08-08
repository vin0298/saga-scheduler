package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/pborman/uuid"
)

type scheduler struct {
	Router    *mux.Router
	DB        *sqlx.DB
	client    client
	metricsDB metricsDB
}

type createContainerRequestData struct {
	Name     string `json:"name,omitempty"`
	Type     string `json:"type,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Server   string `json:"server,omitempty"`
	Alias    string `json:"alias,omitempty"`
}

type client interface {
	executeOperationRequest(req *http.Request) (*operation, error)
}

type agentClient struct{}

func (a agentClient) executeOperationRequest(req *http.Request) (*operation, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer response.Body.Close()

	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	var op *operation

	err = json.Unmarshal(body, &op)
	if err != nil {
		return nil, err
	}

	return op, nil
}

func (s *scheduler) initialize(user, password, dbname, host, port, sslmode string) error {
	connectionString := fmt.Sprintf("user=%s password=%s dbname=%s host=%s port=%s sslmode=%s", user, password, dbname, host, port, sslmode)
	var err error
	s.DB, err = sqlx.Connect("postgres", connectionString)
	if err != nil {
		return err
	}

	s.Router = mux.NewRouter()
	s.Router.HandleFunc("/api/v1/container", s.createNewLxcHandler).Methods("POST")
	s.Router.HandleFunc("/api/v1/container", s.getContainerHandler).Methods("GET")
	s.Router.HandleFunc("/api/v1/container/updatestate", s.updateStateLxcHandler).Methods("POST")
	s.Router.HandleFunc("/api/v1/container", s.deleteLxcHandler).Methods("DELETE")
	s.client = agentClient{}
	s.metricsDB = prometheusMetricsDB{}

	return nil
}

func (s *scheduler) run(port string) {
	log.Fatal(http.ListenAndServe(port, s.Router))
}

func (s *scheduler) getContainerHandler(w http.ResponseWriter, r *http.Request) {
	type resp struct {
		ID      string `json:"id" db:"id"`
		LXDName string `json:"lxd_name" db:"lxd_name"`
		LXCName string `json:"lxc_name" db:"lxc_name"`
		Image   string `json:"image" db:"image"`
		Status  string `json:"status" db:"status"`
	}

	var result []resp
	rows, err := s.DB.Queryx(`SELECT c.id as "id", c.name as "lxc_name", d.name as "lxd_name", c.alias as "image", c.status as "status" FROM lxc c JOIN lxd d ON c.lxd_id = d.id`)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
	}

	for rows.Next() {
		var temp resp
		err = rows.StructScan(&temp)
		if err != nil {
			respondWithError(w, http.StatusBadRequest, err.Error())
		}
		result = append(result, temp)
	}

	respondWithJSON(w, http.StatusOK, result)
}

func (s *scheduler) createNewLxcHandler(w http.ResponseWriter, r *http.Request) {
	var data createContainerRequestData
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&data); err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	defer r.Body.Close()
	lxdInstance, err := s.metricsDB.getLowestLoadLxdInstance()
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	err = lxdInstance.getLxdByIP(s.DB)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	newLxc := lxc{
		ID:         uuid.New(),
		LxdID:      lxdInstance.ID,
		Name:       data.Name,
		Type:       data.Type,
		Alias:      data.Alias,
		IsDeployed: 1,
	}

	err = newLxc.insertLxc(s.DB)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	op, err := s.createNewLxc(data, lxdInstance.Address)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	op.LxcID = newLxc.ID
	err = op.insertOperation(s.DB)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	respondWithJSON(w, http.StatusOK, op)
	return
}

func (s *scheduler) createNewLxc(data createContainerRequestData, lxdIPAddress string) (op *operation, err error) {
	url := fmt.Sprintf("http://%s:9200/api/v1/container", lxdIPAddress)
	payload, err := json.Marshal(data)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	return s.client.executeOperationRequest(req)
}

func (s *scheduler) deleteLxcHandler(w http.ResponseWriter, r *http.Request) {
	type deleteLxcRequest struct {
		ID   string `json:"id,omitempty"`
		Name string `json:"name"`
	}

	var data deleteLxcRequest

	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&data); err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	lxc := lxc{
		ID: data.ID,
	}

	if err := lxc.getLxc(s.DB); err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	lxd := lxd{
		ID: lxc.LxdID,
	}

	if err := lxd.getLxd(s.DB); err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	data.Name = lxc.Name

	url := fmt.Sprintf("http://%s:9200/api/v1/container", lxd.Address)
	payload, err := json.Marshal(data)
	req, err := http.NewRequest("DELETE", url, bytes.NewBuffer(payload))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	op, err := s.client.executeOperationRequest(req)

	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	err = lxc.deleteLxc(s.DB)

	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	respondWithJSON(w, http.StatusOK, op)
}

func (s *scheduler) updateStateLxcHandler(w http.ResponseWriter, r *http.Request) {
	type updateStateRequest struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State struct {
			Action  string `json:"action"`
			Timeout int    `json:"timeout"`
		} `json:"state"`
	}

	var data updateStateRequest

	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&data); err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	lxc := lxc{
		ID: data.ID,
	}

	if err := lxc.getLxc(s.DB); err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	lxd := lxd{
		ID: lxc.LxdID,
	}

	if err := lxd.getLxd(s.DB); err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	url := fmt.Sprintf("http://%s:9200/api/v1/container/updatestate", lxd.Address)
	payload, err := json.Marshal(data)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payload))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	op, err := s.client.executeOperationRequest(req)

	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	respondWithJSON(w, http.StatusOK, op)
}

func respondWithError(w http.ResponseWriter, code int, message string) {
	respondWithJSON(w, code, map[string]string{"error": message})
}

func respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	w.Write(response)
}
