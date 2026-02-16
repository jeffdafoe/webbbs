<?php

namespace App\Door\ZChat\Entity;

use App\Door\ZChat\Repository\SquelchRepository;
use App\Entity\User;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: SquelchRepository::class)]
#[ORM\Table(name: 'zchat_squelch')]
#[ORM\UniqueConstraint(name: 'zchat_unique_user_squelch', columns: ['user_id', 'squelched_user_id'])]
class Squelch
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: false)]
    private User $user;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: false)]
    private User $squelchedUser;

    #[ORM\Column]
    private \DateTimeImmutable $createdAt;

    public function __construct()
    {
        $this->id = Uuid::v7();
        $this->createdAt = new \DateTimeImmutable();
    }

    public function getId(): Uuid
    {
        return $this->id;
    }

    public function getUser(): User
    {
        return $this->user;
    }

    public function setUser(User $user): static
    {
        $this->user = $user;
        return $this;
    }

    public function getSquelchedUser(): User
    {
        return $this->squelchedUser;
    }

    public function setSquelchedUser(User $squelchedUser): static
    {
        $this->squelchedUser = $squelchedUser;
        return $this;
    }

    public function getCreatedAt(): \DateTimeImmutable
    {
        return $this->createdAt;
    }
}
