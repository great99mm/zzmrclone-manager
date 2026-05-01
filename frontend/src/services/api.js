import axios from 'axios';

const API_BASE = process.env.NODE_ENV === 'production' ? '/api' : 'http://localhost:7070/api';
const WS_BASE = process.env.NODE_ENV === 'production' 
  ? `ws://${window.location.host}/ws` 
  : 'ws://localhost:7070/ws';

const api = axios.create({
  baseURL: API_BASE,
  headers: {
    'Content-Type': 'application/json',
  },
});

// Add auth token to requests
api.interceptors.request.use((config) => {
  const token = localStorage.getItem('token');
  if (token) {
    config.headers.Authorization = `Bearer ${token}`;
  }
  return config;
});

export const login = (credentials) => api.post('/login', credentials);
export const register = (data) => api.post('/register', data);
export const changePassword = (data) => api.post('/change-password', data);

export const getTasks = () => api.get('/tasks');
export const getTask = (id) => api.get(`/tasks/${id}`);
export const createTask = (data) => api.post('/tasks', data);
export const updateTask = (id, data) => api.put(`/tasks/${id}`, data);
export const deleteTask = (id) => api.delete(`/tasks/${id}`);
export const startTask = (id) => api.post(`/tasks/${id}/start`);
export const stopTask = (id) => api.post(`/tasks/${id}/stop`);
export const dedupeTask = (id) => api.post(`/tasks/${id}/dedupe`);
export const getTaskLogs = (id, lines = 100) => api.get(`/tasks/${id}/logs?lines=${lines}`);
export const getTaskStatus = (id) => api.get(`/tasks/${id}/status`);

export const getSystemStats = () => api.get('/system/stats');
export const getRcloneStats = () => api.get('/system/rclone-stats');
export const setLogLevel = (level) => api.post('/system/log-level', { level });
export const getSystemLogs = (file = 'system.log', lines = 100) => 
  api.get(`/system/logs?file=${file}&lines=${lines}`);
export const cleanLogs = () => api.post('/system/logs/clean');

export const getRemotes = () => api.get('/rclone/remotes');
export const getRcloneConfig = () => api.get('/rclone/config');

// Output logs (structured persistent format) - requires ?token= query param
export const getOutputLogs = (page = 1, pageSize = 20, taskId = '') => {
  const token = localStorage.getItem('apiToken') || '';
  const tid = taskId ? `&task_id=${taskId}` : '';
  return api.get(`/output-logs?page=${page}&page_size=${pageSize}${tid}&token=${token}`);
};
export const deleteOutputLog = (id) => {
  const token = localStorage.getItem('apiToken') || '';
  return api.delete(`/output-logs/${id}?token=${token}`);
};
export const cleanOutputLogs = (taskId = '') => {
  const token = localStorage.getItem('apiToken') || '';
  const tid = taskId ? `&task_id=${taskId}` : '';
  return api.delete(`/output-logs/clean?token=${token}${tid}`);
};

// Token management
export const getTokenInfo = () => {
  const token = localStorage.getItem('apiToken') || '';
  return api.get(`/token?token=${token}`);
};
export const updateToken = (tokenValue) => {
  const token = localStorage.getItem('apiToken') || '';
  return api.post(`/token?token=${token}`, { token: tokenValue });
};

export const createWebSocket = () => {
  return new WebSocket(WS_BASE);
};

export default api;
