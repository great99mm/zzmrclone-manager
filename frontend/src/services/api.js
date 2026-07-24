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
export const getQuickTasks = () => api.get('/tasks/quick');
export const getTask = (id) => api.get(`/tasks/${id}`);
export const createTask = (data) => api.post('/tasks', data);
export const updateTask = (id, data) => api.put(`/tasks/${id}`, data);
export const deleteTask = (id) => api.delete(`/tasks/${id}`);
export const startTask = (id) => api.post(`/tasks/${id}/start`);
export const pauseTask = (id, mode) => api.post(`/tasks/${id}/pause`, mode ? { mode } : undefined);
export const cancelTask = (id) => api.post(`/tasks/${id}/cancel`);
export const stopTask = (id) => api.post(`/tasks/${id}/stop`);
export const dedupeTask = (id) => api.post(`/tasks/${id}/dedupe`);
export const startProactiveManualMerge = (id) => api.post(`/tasks/${id}/proactive-manual-merge`);
export const closeProactiveUnknownMaintenance = (epochId, data) => api.post(`/proactive-maintenance/${epochId}/close-unknown`, data);
export const getTaskLogs = (id, lines = 100) => api.get(`/tasks/${id}/logs?lines=${lines}`);
export const getTaskStatus = (id) => api.get(`/tasks/${id}/status`);
export const getProactiveStatus = (id, limit = 100, summary = false) => {
  const token = localStorage.getItem('apiToken') || localStorage.getItem('token') || '';
  const summaryParam = summary ? '&summary=true' : '';
  return api.get(`/tasks/${id}/proactive-status?limit=${limit}${summaryParam}&token=${encodeURIComponent(token)}`);
};
export const resolveProactiveBatch = (taskId, resolution) => {
  const token = localStorage.getItem('apiToken') || localStorage.getItem('token') || '';
  return api.post(`/tasks/${taskId}/proactive-resolutions?token=${encodeURIComponent(token)}`, resolution);
};

export const getSystemStats = () => api.get('/system/stats');
export const getRcloneStats = () => api.get('/system/rclone-stats');
export const setLogLevel = (level) => api.post('/system/log-level', { level });
export const getSystemLogs = (file = 'system.log', lines = 100) => 
  api.get(`/system/logs?file=${file}&lines=${lines}`);
export const cleanLogs = () => api.post('/system/logs/clean');

export const getRemotes = () => api.get('/rclone/remotes');
export const getRemoteDetails = () => api.get('/rclone/remotes/detail');
export const getRcloneConfig = () => api.get('/rclone/config');
export const listRemoteDir = (remote, path = '/') => api.get(`/rclone/ls?remote=${encodeURIComponent(remote)}&path=${encodeURIComponent(path)}`);
export const createRemoteDir = (remote, path) => api.post('/rclone/mkdir', { remote, path });

// Mounts
export const getMountSystemInfo = () => api.get('/mounts/system');
export const getMounts = () => api.get('/mounts');
export const getMount = (id) => api.get(`/mounts/${id}`);
export const createMount = (data) => api.post('/mounts', data);
export const updateMount = (id, data) => api.put(`/mounts/${id}`, data);
export const deleteMount = (id) => api.delete(`/mounts/${id}`);
export const startMount = (id) => api.post(`/mounts/${id}/start`);
export const stopMount = (id) => api.post(`/mounts/${id}/stop`);

// File browser
export const listLocalFiles = (path = '/') => api.get(`/files/local?path=${encodeURIComponent(path)}`);
export const listRemoteFiles = (remote, path = '/') => api.get(`/files/remote?remote=${encodeURIComponent(remote)}&path=${encodeURIComponent(path)}`);

// Quick task from file browser
export const createQuickTask = (data) => api.post('/tasks/quick', data);

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
