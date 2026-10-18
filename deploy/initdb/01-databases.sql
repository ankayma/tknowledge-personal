-- One Postgres instance, three databases, three services: no service reads another service's DB.
CREATE DATABASE tk;
CREATE DATABASE cc;
CREATE DATABASE audit;
